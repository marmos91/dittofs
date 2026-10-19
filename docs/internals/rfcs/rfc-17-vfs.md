---
rfc: 17
title: "RFC 17 — the filesystem service (VFS)"
component: vfs
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-18-identity]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-21-nfs]]"
  - "[[rfc-22-smb]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-25-observability]]"
  - "[[rfc-27-namespace-migration]]"
  - "[[rfc-29-test-harness]]"
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

### In plain words

The filesystem service sits between the protocol adapters and everything that
stores data. An adapter is a translator: it turns NFS or SMB messages into plain
calls such as open, write or rename. The service is the one place that runs each
call's checks, always in the same order: may this share be reached, may this
user do this, do others' opens or locks forbid it, is there quota. Picture a
hotel whose guests speak two languages: interpreters at the door translate, and
one front desk checks every request against the same rules.

The service has two halves. The **front** runs on the node that holds the
client's connection: it admits the call to its share and sends it on. The
**shard** half runs at the file's **primary**, the storage node in charge of the
file: it checks opens and locks, reserves quota and drives the storage, with
nothing in between. On a single node both halves are one process and every
crossing is a function call. An adapter never learns which node holds a file.

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
2. The front admits the call to the `profiles` share and sends it to the
   primary of the file's shard. There the shard half checks the open's
   permission and the locks others hold, reserves quota if the file grows, and
   hands the bytes to the engine, which stages them in the journal. alice-pc
   gets OK.
3. build01's write arrives through the NFS adapter as a write on an
   **anonymous open**, which carries no state. The same path checks it against
   the deny modes others hold, finds alice's, and refuses it as a sharing
   violation. The NFS adapter turns that into an NFS error.

One rule, in one place, for both protocols.

**Reaching a share works the same way.** Resolving a share's name or path,
admitting the call, the NFSv4 **pseudo-filesystem** and the list of shares a
client may see all belong to the service. An adapter only turns NFSv3 MOUNT,
NFSv4 PUTROOTFH, SMB TREE_CONNECT, SECINFO and share enumeration into the
service's `Root`, `SecInfo` and `ListShares` calls, and keeps no share list of
its own. Admission is not a door passed once at mount: it runs on every call,
so an NFSv4 client presenting a saved handle meets the same checks as one
walking the pseudo-filesystem into the share. Mounts, tree connects and
sessions are never stored.

![The six steps of every call — admit, route, authorise, check open state, reserve quota, engine — run in one order for both protocols: alice-pc's 64 KiB SMB write through her own open passes all six and gets OK, while build01's NFSv3 write on an anonymous open is refused at step 4 by alice's deny mode as ErrShareViolation, which the NFS adapter turns into an NFS error](img/rfc17-six-steps.svg)

**The words you need.**

- **adapter** — the code for one wire protocol: framing, compounds, replay
  caches, error codes, and nothing else ([RFC 20](rfc-20-adapters.md)).
- **front** / **shard** — the service's two halves: the front on the protocol
  node, the shard at the file's primary ([§2](#2.%20Front%20and%20shard)).
- **primary** — the storage node that owns the file's shard and holds its
  namespace, open state and content; every call that changes the file runs there
  ([§4.8](#4.8%20One%20primary%20per%20file), [glossary](rfc-0-data-lifecycle.md#Glossary)).
- **open** / **anonymous open** — a client's open file, held as state / what an
  NFSv3 read or write runs against instead: no state, the caller's own identity
  ([§4.1](#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection)).
- **admission** — the per-share checks every call passes before the permission
  check: the share's state, the authentication flavours it accepts, its client
  rules, and squashing, which maps the caller to the user it acts as
  ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)).
- **the chokepoint** — the one function in the metadata store that decides file
  permissions; the shard half is its only caller ([§4.6](#4.6%20One%20chokepoint)).
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
  The one wait the service makes inside a call — behind a running clone — is
  bounded by what the adapter allows ([§5.9](#5.9%20Copy%20and%20clone)).
- Adapters never see the cluster: a call sent to the wrong node is re-routed
  inside the service, and a write retried after a lost reply is applied once
  ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)).
- Quota is enforced here and nowhere else, and writers spread over several
  primaries overshoot a hard limit by a stated bound at most.

**How the rest is organised.** §1 gives the purpose and non-goals. §2 is the
architecture: the front, the shard, who owns open state, the wiring and the
package layout. §3 is the interface — value types, operations, callbacks,
events, ID encodings, and every wire request with the calls it makes. §4 holds
the rules, with §4.9 on shares and the pseudo-filesystem. §5, the order of each
operation, is the core. §6 is the invariants with the test of each; §7
metrics; §8 the test layers and benchmarks; §9 how the code gets there; §10
open questions.

### Scenarios

How a call crosses from a protocol node to a storage node, and how a retried
write is answered once, are [RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)'s
and [RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)'s scenarios;
this RFC adds nothing to them. Two that are this RFC's:

**build01 writes `profiles/alice/notes.txt` over NFS while alice-pc holds an SMB lease on it.**

1. The lease is a caching grant, leave for alice-pc to cache the file. The
   shard half sees the conflict and starts a recall, an SMB lease break
   ([§3.2](#3.2%20Callbacks)).
2. It answers build01 with `ErrDelay` at once, which the NFS adapter sends as
   the protocol's retry signal. No worker waits ([§3.2](#3.2%20Callbacks)).
3. build01 retries shortly; once alice-pc acknowledges the break, the write can
   go ahead.
4. If alice-pc never answers, the break is forced at its deadline, 35 s for
   SMB, and the lease drops to none ([§3.2](#3.2%20Callbacks)).

Outcome: a silent client costs others a retry interval, never a stuck server
thread.

**An administrator removes bob's grant on `profiles` while bob-pc still holds handles.**

1. bob-pc's next call presents a handle it got earlier.
2. The front admits the call, and the metadata store's one permission
   function evaluates bob's share grant again, as on every call
   ([§4.6](#4.6%20One%20chokepoint)).
3. No grant admits him now, so the call is refused. Holding a handle grants
   nothing ([§4.6](#4.6%20One%20chokepoint)).
4. Any cache of permission results has the grant in its key, so no stale "yes"
   survives ([§4.6](#4.6%20One%20chokepoint)).

Outcome: losing access takes effect on the next call, not at the next mount.

---

## 1. Purpose

Every client operation touches several components: a write is authorised by the
namespace, checked against open state, charged to a quota, staged in the journal
and later recorded as existence. Something has to put those steps in order. This
document names that something and fixes where it stands:

![The layers: NFS and SMB adapters speak wire only and call one vfs.Service; its front admits and routes, its shard half alone calls the metadata store (RFC 16), the content engine (RFC 8) and open state (RFC 14) at the file's primary, calls adapters back only through the Callbacks interface they implement, and emits quota and access events to a sink it never waits on](img/rfc17-layers.svg)

[RFC 0 §1.3](rfc-0-data-lifecycle.md#1.3%20The%20layers) draws the same picture with the roles marked.

One service, rather than each adapter composing the parts itself, buys four
things:

1. **Orchestration is written once.** Written in each adapter, the orders drift,
   and a check one adapter has and the other lacks is how a client reaches data
   nobody audited.
2. **Cross-protocol rules have one home.** An NFS lock against an SMB deny mode,
   or an NFS write against an SMB lease, is decided where both are visible.
3. **The topology is invisible to adapters.** A call whose primary is another node
   is forwarded by the front; an adapter never learns there was a hop.
4. **It is testable without a protocol.** The service has its own suite (§8),
   and the protocol suites then test translation only.

**The name.** `Filesystem` is taken three ways over: the metadata store's
`FilesystemInfo`, Go's `io/fs.FS`, and the product's own description. The word
for this layer across the field is **VFS** — the protocol-neutral file API
between front-ends and storage. The package is `vfs`, the interface
`vfs.Service`, and an adapter holds one value of it. Call sites read
`svc.Open(…)`, never `vfs.VFS`.

### 1.1 Non-goals

The service **MUST NOT**:

- encode or decode a wire format, run a compound or keep a per-session replay
  cache — those are the adapters' ([RFC 20 §2.2](rfc-20-adapters.md#2.2%20What%20an%20adapter%20may%20hold));
- evaluate a file permission itself — it asks the one chokepoint ([§4.6](#4.6%20One%20chokepoint));
- hold a copy of metadata, open state or content that outlives the call that
  read it;
- persist anything of its own. Every durable fact is recorded by the component
  that owns it;
- import an adapter. Calls upward go through interfaces adapters implement
  ([§3.2](#3.2%20Callbacks));
- count or budget the requests an adapter holds pending: that is the adapter's
  ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)) and its memory
  RFC 24's;
- answer DFS referrals or NFSv4 referrals to another installation. A share is
  served only by the installation that holds it.

## 2. Front and shard

### 2.1 What runs where

[RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut) puts the service on the
protocol node, because one client call often spans shards. [§4.8](#4.8%20One%20primary%20per%20file)
requires the conflict check, the quota reservation and the engine's write to
run at the file's primary with no hop between them. Both hold, because the
service is two halves with one interface between them:

| Half | Package | Runs on | Does | Holds past a call |
| --- | --- | --- | --- | --- |
| **front** | `internal/vfs` | the protocol node holding the connection | admission ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)); routing a handle to its shard ([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20primaries)); `Root`, `SecInfo`, `ListShares` and the pseudo-filesystem; work that spans shards — a rename or link across two, a listing whose entries live in several, a copy between two; client leases and the callback registry ([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)) | nothing about files: per-call counts only, and the clients it is lease owner of |
| **shard** | `internal/vfs/shard` | every node, for store reads; the file's primary, for everything else | every call to the metadata store and the engine; the orchestration of §5, one coarse operation per file; the chokepoint call; quota reservations; the count of admitted calls a share removal joins; grace; the lock order | quota reservations, admitted-call counts, open state ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |

**One component calls the metadata store and the engine.** The front imports
neither and makes no store call. What it needs from the store — share records,
export policies, netgroups and the share list's version for admission, the share
path index for the pseudo-filesystem and `Root`, principals and grants for
identity resolution — it reads through the shard package's **catalog**, the
store-wide reads that belong to no shard, served on every node
([RFC 15 §4](rfc-15-topology.md#4.%20Where%20each%20call%20runs)'s "any node"
row). Per-file work goes through `Shard`, routed to the file's primary.

> decision: the service is a new layer, not a grown metadata store. The
> metadata store shrinks to store operations beneath it, and RFC 16's store stays
> swappable under the shard half; the orchestration of §5 lives in the shard,
> and only the shard imports the store and the engine. Rejected: growing
> today's `pkg/metadata.Service` into the service, which keeps orchestration and
> storage in one package a second backend would have to reimplement; and
> rewriting both in one go, which no reviewer can follow (§9). Overturned if a
> second store backend is dropped from the plan, so nothing below the shard
> needs to stay swappable.

> decision: one component calls the metadata store and the engine, so a
> transaction boundary, a retry, a fence and a cache key are each decided in
> one place. Two callers would each pick a different retry for the same
> conflict and key their caches on different fields. Overturned only if a read
> path is measured needing a store read that going through the shard package
> makes slower.

### 2.2 Who owns open state and locks

| Who | Holds | Never |
| --- | --- | --- |
| **adapter** ([RFC 20](rfc-20-adapters.md)) | the wire form of a waiting lock: the request to answer, the NLM host to call back | decides a conflict |
| **front** (protocol node) | each client's lease record and callback channel ([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease), [RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)); relays recalls and grants to the connection | any of the open-state table: it routes `Lock`, `Open`, `Close` and the rest to the file's primary |
| **shard** (the file's primary) | [RFC 14](rfc-14-open-state.md)'s open-state table — opens, deny modes, byte-range locks of NLM, NFSv4 and SMB alike, delete pending, caching grants, watches, lock waiters, and in a cluster layouts — as the concrete `*openstate.Table` in `shard.Deps`; runs the conflict check and the I/O it admits under the file's accept lock with nothing between ([§4.8](#4.8%20One%20primary%20per%20file), V7) | — |
| **metadata store** ([RFC 16](rfc-16-metadata-store.md)) | only what must survive a restart: client records, persistent SMB opens, the durable open records that keep an unlinked file alive | an ordinary lock |

The table is in memory, keyed by file — never by name or handle. It is lost when
a primary fails over and rebuilt by grace reclaims for that shard alone
([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)).
A lock costs one hop to the primary and no store write. One shard's locks are
decided by one node, so locking scales across shards, not within one hot shard.

> decision: there is no separate distributed lock manager. The conflict check
> must sit with the I/O it gates, at the primary, or something can be granted
> between them ([§4.8](#4.8%20One%20primary%20per%20file)); a lock manager on
> other nodes would add a hop and a window to every write. Overturned if one
> shard's lock traffic is measured saturating its primary while the files it
> serves cannot be split across shards ([RFC 11](rfc-11-ownership.md)).

### 2.3 Dependencies and wiring

Each half is built once by the composition root
([RFC 15 §2.4](rfc-15-topology.md#2.4%20Composition%20by%20role)) from a `Deps`
struct. A field is an interface where it has two implementations and a
concrete type where it has one, as in
[RFC 20 §3.4](rfc-20-adapters.md#3.4%20Interface). The front declares what it
calls, beside its own code:

```go
package vfs // internal/vfs

// Shard is the per-file half (§2.1): one coarse method per §5 operation, each
// taking the resolved Caller (RFC 18 §6.1) in place of the Identity. Its two
// implementations are shard.Local, which runs §5, and the shard service's
// network client (RFC 15 §2.5).
type Shard interface {
	Open(ctx context.Context, c Caller, req OpenRequest) (OpenResult, error)
	Write(ctx context.Context, c Caller, ref OpenRef, off int64, src []byte, s Stability) (WriteResult, error)
	Read(ctx context.Context, c Caller, ref OpenRef, off int64, dst []byte) (int, bool, error)
	Name(ctx context.Context, c Caller, op NameOp) (NameResult, error) // create, link, unlink, rmdir, rename within one shard; a coordinator's prepare and commit across two (RFC 15 §4.2)
	GetAttr(ctx context.Context, c Caller, f FileID) (metadata.File, error)
	SetAttr(ctx context.Context, c Caller, t Target, a AttrChange) (WCC, error)
	Lock(ctx context.Context, c Caller, ref OpenRef, r LockReq) error
	// … one method for each remaining per-file operation of §3.1
}

// Catalog is the store-wide reads that belong to no shard, served on every
// node (RFC 15 §4): shares, export policies, netgroups, the share list's
// version, principals and grants, the reads of a file any node may serve —
// Lookup, Entries, ACL, Xattrs, Streams, Granted, Resolve, Capacity — and RFC
// 14's client-record calls the lease owner makes (RFC 14 §4.1).
// Implemented by shard.Catalog and by the recording fake.
type Catalog interface {
	Share(ctx context.Context, ref ShareRef) (Share, error)
	ShareList(ctx context.Context) (ShareList, error)
	Resolve(ctx context.Context, c Caller, h Handle) (FileID, error)
	Lookup(ctx context.Context, c Caller, dir Handle, name []byte) (Handle, metadata.File, error)
	// … the remaining any-node reads
}

// Router finds a file's primary: route.Single on one node, a cache of shard
// records in a cluster (RFC 15 §6).
type Router interface {
	Shard(ctx context.Context, f FileID) (Shard, error)
}

// Recaller is how a primary reaches the lease owner's Callbacks: in process
// on one node, over a node channel in a cluster (RFC 15 §4.4).
type Recaller interface {
	Recall(ctx context.Context, c ClientID) (Callbacks, error)
}

type Deps struct {
	Shards   Router
	Catalog  Catalog
	Identity identity.Resolver // RFC 18 §6: real, or a fake in the front's suite
	Clients  *Clients          // leases and the callback registry; also the local Recaller
	Events   Events            // the sink, or the null sink
	Clock    clock.Clock
	Metrics  *metrics.Registry
	Logger   *slog.Logger
}
```

```go
package shard // internal/vfs/shard

type Deps struct {
	Meta    *metadata.Store  // RFC 16 §3.2: the views the shard declares
	Open    *openstate.Table // RFC 14: this primary's open-state table (§2.2)
	Engine  *engine.Engine   // RFC 8
	Recall  vfs.Recaller
	Events  vfs.Events
}
```

**One node, wired.** The whole single-node composition of the service, between
the stores and the adapters:

```go
st, _ := metadata.Open(ctx, kv, metadata.Options{ /* … */ })
eng, _ := engine.New(engine.Deps{ /* RFC 8 §2 */ })
ost := openstate.New(openstate.Deps{Store: st /* … */})
cat := shard.NewCatalog(st, ost)
clients := vfs.NewClients(cat)
loc := shard.NewLocal(shard.Deps{Meta: st, Open: ost, Engine: eng, Recall: clients, Events: sink})
svc := vfs.New(vfs.Deps{Shards: route.Single(loc), Catalog: cat, Identity: ident,
	Clients: clients, Events: sink, Clock: clk, Metrics: reg, Logger: log})
nfsSrv, _ := nfs.New(core.Deps{Service: svc /* … */}, cfg.NFS)
smbSrv, _ := smb.New(core.Deps{Service: svc /* … */}, cfg.SMB)
```

In a cluster the protocol node passes a `Router` over its shard-record cache,
which returns the shard service's client for a remote primary and `loc` for a
shard this node is primary of; a node without `storage` has no `loc`, and its
catalog's open state is RFC 14's client-record half alone. The rest is
unchanged.

**Interfaces, and why each exists.** `vfs.Service` (the service, the recording
fake, the null service — [RFC 20 §2.5](rfc-20-adapters.md#2.5%20Architecture%20and%20package%20layout));
`Shard` (local, remote); `Catalog` (the shard package's, the fake); `Router`
(one node, cluster); `Events` (sink, null); `identity.Resolver` (RFC 18's, a
fake); `Recaller` (local, node channel); `Callbacks` (one per adapter). The
metadata store, the open-state table and the engine are concrete types: the
shard package is their only consumer and declares only the views it calls
([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)).

### 2.4 Package layout

```text
pkg/vfs/                    the contract adapters build against
  service.go                the Service interface (§3.1), with a section comment per area
  types.go                  Handle, Identity, OpenRef, Target, Access, … and the aliases of metadata types (§3.0)
  open.go                   OpenRequest, OpenResult
  ids.go                    the codecs of every client-held name (§3.4)
  errors.go                 sentinels, *Delay, *QuotaError, *StateError, *CrossError, Errors() (§4.3);
                            re-exports, as aliases, the neutral content errors the engine also returns
  callbacks.go              Callbacks, Revocation (§3.2)
  events.go                 Events and the event types (§3.3)
pkg/vfs/vfstest/            recording fake, null service, recording Callbacks — generated (§8)
  suite/                    suite.Run: the conformance suite, one file per area (§8)
internal/vfs/               the front: one package, a file per concern
  service.go                New(Deps); the Service methods, each admit → route → call
  route.go                  Shard, Catalog, Router, Recaller: the interfaces the front declares
  admit.go                  the four admission checks; the per-share in-flight count (§4.9)
  shares.go                 Root, SecInfo, ListShares (§4.9)
  pseudo.go                 the pseudo-filesystem, a pure function of the share list
  coordinate.go             work across shards: rename, link, listings, copy (§4.8)
  clients.go                leases and the callback registry; the local Recaller
  callbacks.go              relays a primary's recalls and grants to the adapter
internal/vfs/shard/         the per-file half: one package
  local.go                  NewLocal(Deps): the Shard every primary runs
  catalog.go                NewCatalog: the store-wide reads, on every node
  authorize.go              the only file that calls Files.Authorize or Granted; emits access events
  lockorder.go              the lock order (§5.0)
  write.go read.go          §5.1, §5.2
  open.go                   open, close, release (§5.3)
  namespace.go              create, link, unlink, rmdir, rename (§5.4, §5.10)
  attr.go                   set-attribute, ACLs, xattrs, size changes (§5.5)
  quota.go                  reservations and slack (§5.6)
  copy.go hold.go           copy, clone and the wait behind a running clone (§5.9)
  lock.go                   locks, grants, watches (§5.11)
  recall.go release.go      recalls through the Recaller; the four moments of release (§5.3)
  verifier.go               passes the engine's verifier through (§5.8)
internal/tools/genvfstest/  the go/types generator behind vfstest
```

- **Who calls the store and the engine.** Within `internal/vfs/shard`, calls to
  the engine and to the metadata store's write views appear only in the
  orchestration files — `write.go`, `read.go`, `open.go`, `namespace.go`,
  `attr.go`, `copy.go`, `lock.go`, `release.go` — and the store's read views in
  those and `catalog.go`; `authorize.go` is the one caller of the chokepoint.
  The import test (§8) checks it: `internal/vfs` imports neither the metadata
  store, open state nor the engine, only `Shard` and `Catalog`; and within the
  shard package a call of the engine or a write view outside those files fails
  it, naming the file.
- **One package per half, a file per concern.** The concerns call each other
  — admission feeds routing, a rename needs the lock order and the release —
  and one package has no import cycles to manage, as RFC 20's `core` reasons.

> ponytail: the pure leaves — the pseudo-filesystem builder, admission over a
> (share, identity), quota arithmetic — are files, not packages. Move one into a
> subpackage of its own when its file passes 500 lines; its tests then import
> nothing but the leaf.

## 3. Interface

Signatures are indicative; the obligations are normative.

### 3.0 Value types

Every type an adapter passes or receives is in `pkg/vfs`. Types the metadata
store and open state define — `ClientID`, `OpenID`, `GrantID`, `WatchID`,
`SessionRef`, `StateRef`, `LockOwner`, `ByteRange`, `File`, `Attrs`, `ACL`,
`Xattr`, `Principal`, `FilesystemInfo`, `Usage`, `Quota`, `CachingGrant`,
`LockWaiter`, `ChangeMask`, `ShareID` — are re-declared there as aliases
(`type File = metadata.File`), so an adapter imports one package and the
import rule of [RFC 20 §2.3](rfc-20-adapters.md#2.3%20What%20an%20adapter%20may%20import)
holds.

| Type | What it is |
| --- | --- |
| `Identity` | what the adapter authenticated, not yet mapped through any share's policy: `Protocol`, the flavour and Kerberos level, the uid, gid and groups an `AUTH_SYS` credential carried, the client's address, and RFC 18's proof fields — `Name`, `Principal`, `SIDs`, `DenyOnly`, `Machine`, `Expires` ([RFC 18 §3.1](rfc-18-identity.md#3.1%20The%20authentication%20interface)), never the session key. Every method takes one; the front resolves it per share ([RFC 18 §6.1](rfc-18-identity.md#6.1%20From%20Identity%20to%20caller)) |
| `Protocol` | `NFS3`, `NFS4`, `NLM`, `SMB` |
| `AuthFlavor` | a flavour a share admits: `sys`, `krb5`, `krb5i`, `krb5p`, `ntlm`, `none` |
| `Handle` | opaque bytes naming a file or a pseudo directory (§3.4, [§4.4](#4.4%20Handles%20are%20opaque)) |
| `OpenRef` | `{Client ClientID; Open OpenID; Handle Handle; Kind RefKind}`: an open, a delegation, or an anonymous open. It carries no identity: the call's `id` is compared with the principal the open was granted to (§3.1). `vfs.Anonymous(h)` builds an anonymous one, `vfs.Delegated(c, g, h)` one for I/O under a delegation |
| `Target` | `{H Handle; Open *OpenRef}`: what a mutation acts on — the file for an attribute change, the directory for a name change — and, when set, the open the request came through, whose grant authorises it ([§4.6](#4.6%20One%20chokepoint)) |
| `Cursor` | where a listing resumes: the 63-bit digest of the last entry returned ([RFC 7 §3.5](rfc-7-namespace-metadata.md#3.5%20A%20cookie%20survives%20concurrent%20mutation)), zero to start |
| `DirEntry`, `Page`, `EntryFields` | one listed entry — name, handle, its digest as cursor, and its `File` when asked; a page `{Entries []DirEntry; Next Cursor; EOF bool}`; which fields a listing fills |
| `CreateArgs`, `CreateKind` | `{Kind; Attrs; Verifier *[8]byte; Target []byte; Dev uint64}`; `Dir`, `Symlink`, `Fifo`, `Socket`, `Char`, `Block`, `Regular` |
| `OpenRequest`, `OpenResult` | disposition, access, deny, the caching grant wanted, durability, delete on close, create GUID and app instance, reclaim, a reconnect's match, a snapshot token, the parent and name it resolves through, `CreateArgs` for a create / the `OpenRef`, the `File`, `MaximalAccess`, the grant given, `Created`, and the parent's `WCC` when it created |
| `AttrChange` | `metadata.Attrs` plus `Guard *time.Time` (a ctime guard, [RFC 20 §2.4](rfc-20-adapters.md#2.4%20Protocol%20details%20that%20stay%20in%20the%20adapter)), `DeleteOnClose *bool` and `Suspend`, `Resume` time masks — the last two only through an open |
| `XattrMode` | `Create`, `Replace`, `Either` |
| `Access` | an access mask in RFC 7's terms ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)) |
| `Stability` | `Unstable`, `DataSync`, `FileSync` |
| `WCC` | `{Before *File; After File}`: one object's attributes either side of a mutation, from one hold of its accept lock; `Before` nil where it could not be captured so ([§5.7](#5.7%20GetAttr)) |
| `WriteResult` | `{N int; Verifier Verifier; WCC WCC}` |
| `CreateResult`, `UnlinkResult`, `RenameResult` | `{Handle; File; Dir WCC}`; `{Dir WCC}`; `{From, To WCC; Moved *WCC}` — `Moved` when the moved file's own attributes changed |
| `LockReq` | `{Ranges []ByteRange; Exclusive, Wait, Reclaim bool; Owner LockOwner; Seq *LockSequence}` (§5.11) |
| `RenameOpts` | `{NoReplace bool}` |
| `CopyArgs`, `CopyResult` | `{Src, Dst OpenRef; SrcOff, DstOff, Len int64}`; `{N int64; WCC WCC}` |
| `SeekWhat` | `Data`, `Hole` |
| `GrantKind` | `None`, `Read`, `ReadWrite`, `ReadHandle`, `ReadWriteHandle`, and an NFS delegation's read or write |
| `WatchSpec` | `{Dir Handle; Recursive bool; Filter ChangeMask}` |
| `ShareRef`, `ShareInfo` | a share named as the client sent it — an NFS path or an SMB name; the zero value is the pseudo-root / a share's name, `Flavors []AuthFlavor`, comment, `Hidden`, the root's `MaximalAccess`, and the share policy's SMB flags (`ExportPolicy.SMB`: encrypt, continuously available, [RFC 16](rfc-16-metadata-store.md)) |
| `Change`, `Op` | one change a watch reports; the operation an event or metric names |
| `ClientInfo`, `ClientStatus` | what `Connect` registers — protocol, owner, verifier, principal, NSM monitor name; what `Renew` reports (§3.1) |
| `Delay`, `QuotaError`, `StateError`, `CrossError` | the typed errors of [§4.3](#4.3%20Errors%20are%20neutral%20values) |

### 3.1 Operations

One interface. A consumer that needs less declares a narrower interface of its
own beside its code; the service declares no groups.

```go
package vfs

type Service interface {
	// ── Clients (RFC 14). The lease owner is the front of the node holding the
	// client's sessions (§2.2).
	Connect(ctx context.Context, id Identity, c ClientInfo, cb Callbacks) (ClientID, error)      // SETCLIENTID_CONFIRM, the confirming CREATE_SESSION, SMB NEGOTIATE, an NLM host's first call; again on another node to re-register the callback path: the latest wins (RFC 14 Rebind)
	CreateSession(ctx context.Context, id Identity, c ClientID, seq uint32, reply []byte) ([]byte, error) // a replay returns the cached reply, seq+1 records this one, any other StateError SeqMisordered
	Renew(ctx context.Context, id Identity, c ClientID) (ClientStatus, error)
	Disconnect(ctx context.Context, id Identity, c ClientID, s SessionRef) error // an SMB session's last channel ended
	Destroy(ctx context.Context, id Identity, c ClientID) error                   // DESTROY_CLIENTID; StateError ClientBusy while state remains
	ReclaimComplete(ctx context.Context, id Identity, c ClientID) error
	FreeRevoked(ctx context.Context, id Identity, c ClientID, s StateRef) error
	TestState(ctx context.Context, id Identity, c ClientID, s []StateRef) []error // TEST_STATEID: one answer per stateid, nothing changed
	ReleaseLockOwner(ctx context.Context, id Identity, c ClientID, o LockOwner) error
	HostRestarted(ctx context.Context, id Identity, host []byte, state uint32) error // NSM SM_NOTIFY, NLMPROC4_FREE_ALL: expires the host's old client

	// ── Names. Every method also takes a pseudo-filesystem handle (§4.9).
	Lookup(ctx context.Context, id Identity, dir Handle, name []byte) (Handle, File, error)
	ReadDir(ctx context.Context, id Identity, dir Handle, after Cursor, max int, want EntryFields) (Page, error)
	Create(ctx context.Context, id Identity, dir Handle, name []byte, a CreateArgs) (CreateResult, error) // mkdir, symlink, mknod, NFSv3 CREATE
	Link(ctx context.Context, id Identity, dir Handle, name []byte, target Handle) (CreateResult, error)
	Unlink(ctx context.Context, id Identity, dir Target, name []byte) (UnlinkResult, error) // any entry; a directory only when empty
	Rmdir(ctx context.Context, id Identity, dir Target, name []byte) (UnlinkResult, error)  // a directory only; ErrNotDir otherwise
	Rename(ctx context.Context, id Identity, from Target, fromName []byte, to Handle, toName []byte, o RenameOpts) (RenameResult, error)

	// ── Attributes, ACLs, xattrs, streams.
	GetAttr(ctx context.Context, id Identity, h Handle) (File, error)
	SetAttr(ctx context.Context, id Identity, t Target, a AttrChange) (WCC, error)
	GetACL(ctx context.Context, id Identity, h Handle) (ACL, error)
	SetACL(ctx context.Context, id Identity, t Target, acl ACL) (WCC, error)
	Xattrs(ctx context.Context, id Identity, h Handle) ([]Xattr, error)
	SetXattr(ctx context.Context, id Identity, t Target, x Xattr, m XattrMode) (WCC, error)
	RemoveXattr(ctx context.Context, id Identity, t Target, name []byte) (WCC, error)
	Streams(ctx context.Context, id Identity, h Handle) ([]File, error)
	Access(ctx context.Context, id Identity, h, parent Handle, want Access) (Access, error) // Files.Granted ∩ want; parent zero when unknown

	// ── Data.
	Open(ctx context.Context, id Identity, req OpenRequest) (OpenResult, error)
	Downgrade(ctx context.Context, id Identity, ref OpenRef, access, deny Access) error // NFSv4 OPEN_DOWNGRADE
	ConfirmOpen(ctx context.Context, id Identity, ref OpenRef, seq uint32) error        // NFSv4.0 OPEN_CONFIRM
	Close(ctx context.Context, id Identity, ref OpenRef) (*WCC, error)                    // the parent's WCC when a delete on close ran
	Read(ctx context.Context, id Identity, ref OpenRef, off int64, dst []byte) (n int, eof bool, err error)
	Write(ctx context.Context, id Identity, ref OpenRef, off int64, src []byte, s Stability) (WriteResult, error)
	Commit(ctx context.Context, id Identity, ref OpenRef, off, length int64) (Verifier, error) // NFS COMMIT, SMB FLUSH
	Deallocate(ctx context.Context, id Identity, ref OpenRef, off, length int64) (WCC, error)
	Seek(ctx context.Context, id Identity, ref OpenRef, off int64, what SeekWhat) (int64, error)
	Copy(ctx context.Context, id Identity, a CopyArgs) (CopyResult, error) // COPY, copychunk: bounded chunks, synchronous, may be short
	Clone(ctx context.Context, id Identity, a CopyArgs) (WCC, error)        // CLONE, duplicate-extents: atomic

	// ── Locks, grants, watches (RFC 14).
	Lock(ctx context.Context, id Identity, ref OpenRef, r LockReq) error
	CancelLock(ctx context.Context, id Identity, ref OpenRef, r ByteRange) (granted bool, err error)
	TestLock(ctx context.Context, id Identity, ref OpenRef, r ByteRange, exclusive bool) (*Lock, error)
	Unlock(ctx context.Context, id Identity, ref OpenRef, rs []ByteRange) error // in order; stops at the first range not held, ErrRangeNotLocked
	AckBreak(ctx context.Context, id Identity, c ClientID, g GrantID, to GrantKind) error // to GrantNone: DELEGRETURN, a lease released
	Watch(ctx context.Context, id Identity, c ClientID, w WatchSpec) (WatchID, error)
	Unwatch(ctx context.Context, id Identity, c ClientID, w WatchID) error

	// ── Shares and volumes (§4.9).
	Root(ctx context.Context, id Identity, s ShareRef) (Handle, ShareInfo, error) // MNT, PUTROOTFH, PUTPUBFH, TREE_CONNECT; zero ShareRef: the pseudo-root
	SecInfo(ctx context.Context, id Identity, dir Handle, name []byte) ([]AuthFlavor, error)
	ListShares(ctx context.Context, id Identity) ([]ShareInfo, error)
	StatFS(ctx context.Context, id Identity, h Handle) (FilesystemInfo, error)                // the share's, or the tighter of it and h's project
	Quota(ctx context.Context, id Identity, h Handle, p Principal) (Usage, Quota, error)      // p's own, else the share's default for its kind
	Snapshots(ctx context.Context, id Identity, h Handle) ([]SnapshotInfo, error)             // the snapshots visible at h's path (RFC 12 §2.5)

	// ── Principals (RFC 18 §4.1). No Identity: they act on no share and are not admitted.
	Spell(ctx context.Context, p Principal, scheme identity.Scheme) (identity.Spelling, error)
	Find(ctx context.Context, s identity.Spelling) (Principal, error)
}
```

`Spell` and `Find` are [RFC 18 §4.1](rfc-18-identity.md#4.1%20Two%20directions%2C%20one%20mapping)'s
`Identities`, carried as methods of the one interface.

**Every mutating call returns the attributes the reply needs.** Each returns the
`WCC` of every object it changed, captured as [§5.7](#5.7%20GetAttr) says: a
create or link the new file's `File` and the parent's `WCC`; an unlink or rmdir
the parent's; a rename both directories', and the moved file's when its own
attributes changed; an attribute, ACL or xattr change the file's; a deallocate,
copy or clone the destination's; an open that created, the file and the
parent's, beside `MaximalAccess`; a close that ran a delete on close, the
parent's. An NFSv4 `change_info4` is the adapter's, derived from a directory
`WCC`'s change attributes and never reported atomic
([RFC 21 §9.2](rfc-21-nfs.md#9.2%20Close-to-open%2C%20and%20what%20the%20server%20guarantees)).
So no handler follows a mutation with a `GetAttr`
([RFC 20 §4.3](rfc-20-adapters.md#4.3%20A%20handler%20makes%20at%20most%20one%20mutating%20call)).

> decision: the attributes are always returned, and atomic when present.
> `After` is always set; `Before` only when captured in the same accept-lock
> hold as the change, so it is absent for a write that waited on a recall, for a
> call forwarded to another primary, for a contended lock, and always for a
> directory over NFSv3 ([RFC 21 §11](rfc-21-nfs.md#11.%20Invariants)'s N19).
> Rejected: a separate `GetAttr`, which is not atomic with the change and lets
> another client's write appear as this one's; and an optional flag asking for
> them, which every mutating handler would set. Overturned if returning `After`
> is measured costing a mutation-heavy workload more than the `GetAttr` it
> replaces.

**Every open-state call names the client it acts for**, and the service **MUST**
refuse, with `StateError` `NotYours`, an `OpenID`, `GrantID`, `WatchID` or lock
owner the named client does not hold. A leaked or forged ID then reaches
nothing, and a client whose lease expires releases its watches with the rest of
its state ([RFC 14 §4.3](rfc-14-open-state.md#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)).

**Naming the right client is not enough.** Where a protocol names the client
only through the state it presents, as an NFSv4.0 stateid does, anyone who
learns that state could present it. An open's open-time grant
([§4.6](#4.6%20One%20chokepoint)) therefore authorises a call only when the
call's `id` resolves to the principal the open was granted to, or, for an
NFSv4.1 client under machine-credential state protection, the client's machine
principal for the operations it protects. A call by any other principal through
the same open or lock state **MUST** be refused with `NotYours`
([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), whatever that principal could
do to the file on its own: the state is the opener's, and a principal that wants
its own access opens the file itself.

**A delegation is the client's, not an opener's.** An NFSv4 client may do I/O
under its delegation's stateid for any of its open-owners and principals
(RFC 8881 §9.1.3), so an `OpenRef` naming a delegation is not bound to a
principal. The service **MUST** refuse it with `NotYours` unless the delegation
is held by the call's client — for NFSv4.1 the session's — and **MUST** then
authorise the call as for an anonymous open: `id`'s principal against the file
and the share grant ([§4.6](#4.6%20One%20chokepoint)), and against the deny
modes held. The delegation lends no access.

> decision: `Unlink` removes any entry, an empty directory included, and `Rmdir`
> only a directory. NFSv4 `REMOVE` and an SMB delete name an entry without its
> type, and a handler that looked the type up first would race the entry it
> removes; NFSv3 `RMDIR` is the one request that must refuse a non-directory.
> Split further only if a protocol is found that must refuse a directory on
> its remove.

> decision: there is no space reservation, so `Allocate` is not offered. NFS
> `ALLOCATE` is answered not supported, and an SMB allocation size is accepted
> and changes nothing but what the adapter reports. A reservation would promise
> that a later write cannot fail for space, which a journal shared by many shares
> and drained by offload cannot keep. Add one, as a per-share charge against the
> journal's capacity, if a workload is shown to need `ALLOCATE`'s guarantee.

> decision: no pre-warm and no layouts. Fetching a tree ahead of its readers is
> an operator's action, one of [RFC 23](rfc-23-control-plane.md)'s management
> operations over the engine's `PreWarm`, not a client's. pNFS layouts are out
> of `vfs.Service` until a pNFS design is scheduled: a single node advertises
> none, so every NFSv4.1 layout operation is answered `NFS4ERR_NOTSUPP` from the
> adapter's table with no service call
> ([RFC 21 §2.1](rfc-21-nfs.md#2.1%20Versions%20served)), and RFC 14's layout
> calls are reserved for a cluster. Rejected: layout operations in the service
> now, for a design that does not exist; and an adapter-side protocol over the
> data calls, since a layout is open state at the primary. A separate `Layouts`
> interface joins the service when a pNFS RFC lands (§10).

### 3.2 Callbacks

```go
// Callbacks is implemented by each adapter and registered per client at
// Connect. The service calls up through it and never imports an adapter.
type Callbacks interface {
	RecallGrant(ctx context.Context, g CachingGrant, to GrantKind) error // NFS CB_RECALL, SMB lease or oplock break
	Notify(ctx context.Context, w WatchID, changes []Change) error      // SMB CHANGE_NOTIFY, NFS directory notifications
	Revoked(ctx context.Context, what Revocation)                       // state ended without the client asking: the adapter answers what it holds pending on it
	LockWaitOver(ctx context.Context, w LockWaiter, granted bool) (WaitAnswer, error) // NLM_GRANTED, SMB lock completion (granted); NFSv4.1 CB_NOTIFY_LOCK, optional
}

// Revocation names state the service ended without the client asking. Share
// is set for RevokedShare, where only that share's state ends; Grant for
// RevokedGrant; Shard for RevokedShardState (RFC 14 §4.4). RevokedLeaseExpired
// and RevokedByAdmin end every piece of state the client holds (RFC 14 §4.3,
// §5.3). Until the client frees what was revoked (FreeRevoked), Renew reports
// it by cause in ClientStatus, and a call naming it is refused ErrRevoked.
// A removed or moved share's state is the exception: DropShare deletes the
// records that could list it, so a call naming it is StateError BadState and
// no flag is set, since nothing could ever clear one (RFC 16 §2.3.2, RFC 14).
type Revocation struct {
	Client ClientID
	Share  ShareID
	Grant  GrantID
	Shard  ShardID
	Reason RevokeReason
	Addr   netip.Addr // RevokedShare: non-zero when the share moved and the move names an address (RFC 27 §2.2)
}

type RevokeReason uint8

const (
	RevokedLeaseExpired RevokeReason = iota // the client's lease expired
	RevokedByAdmin                          // an administrator revoked the client; its record and sessions are kept (RFC 14 §5.3)
	RevokedShare                            // RFC 14 DropShare: the share was removed, or moved away when Addr or the move says so
	RevokedGrant                            // one caching grant whose recall missed its deadline
	RevokedShardState                       // a shard's open state was lost
)

type WaitAnswer uint8

const (
	WaitAcked   WaitAnswer = iota // NLM: the host acknowledged NLM_GRANTED; SMB and NFSv4: the completion or notification was sent
	WaitRefused                   // NLM: the host refused the grant; the primary releases the lock
)
```

`ClientStatus` is what `Renew` reports: `ReclaimNeeded` (a shard of the client's
state restarted its grace), `AdminRevoked` and `RecallableRevoked` (a stateid of
that cause is in its revoked set, not yet freed), the lease owner's aggregate of
what each primary reports ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).

`LockWaitOver` returns an error when the client gave no answer by the call's
deadline. For an NLM grant the service then calls it again, and keeps the lock:
a grant is never released for a silence, only for a refusal, a cancel, the
host's restart, an administrator's revoke of its client, or its share's removal
([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)).

A callback **MUST NOT** block the service beyond its context's deadline, and a
callback that fails or times out **MUST** be treated as the client not answering:
a recall that is not acknowledged by its deadline revokes the grant
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).

![A recall without a waiting worker: build01's NFS write conflicts with alice-pc's SMB lease, so the service sends RecallGrant, which the SMB adapter turns into a lease break, and answers ErrDelay at once, mapped to NFS4ERR_DELAY or NFS3ERR_JUKEBOX; alice-pc acknowledges, the service gets AckBreak, and build01's retry goes ahead. If alice-pc never answers, the break is forced at 35 s and the lease drops to none; if the conflicting caller is an SMB client, the SMB adapter sends an interim response and re-drives the call itself, held up to smb_pending_deadline, which exceeds the break deadline](img/rfc17-recall.svg)

**No worker waits on a client.** An operation that conflicts with a caching
grant starts the recall and returns `ErrDelay` at once. An NFS adapter answers
the protocol's retry signal and the client retries. SMB has no retry signal —
its client does not resend a request answered with an interim response — so
the SMB adapter holds the request and re-drives it itself, a mechanism of its
request pipeline ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)).
The service's one wait inside a call is behind a running clone or copy chunk,
bounded by the hold the adapter allows (`vfs.WithHold`, [§5.9](#5.9%20Copy%20and%20clone)).

> decision: the service classifies and the adapter holds. The service names a
> delay's class (`Delay.Hold()`) and bounds only its own wait behind a clone;
> the adapter's pipeline (`core/hold.go`) holds and re-drives — `STATUS_PENDING`,
> a timer, no worker, the resolve and call run again, never in a handler — and
> RFC 24's hold pool bounds the bytes held. Rejected: the service parking wire
> requests, which cannot send `STATUS_PENDING`; per-handler re-drive, which is
> today's and drifts per command; and a hold-policy hook in the context, which
> made the service count what only the adapter holds. Overturned if a protocol
> needs a hold the service alone can see the end of.

**How long a delayed request may be held.** Every `ErrDelay` is a `*Delay` whose
`Hold()` names its class; an adapter that holds requests chooses the bound by
the class, never by the operation, and the final status by the cause
([RFC 22 §6.6](rfc-22-smb.md#6.6%20Service%20errors%20on%20the%20wire)). The
**break deadline** is [RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)'s
35 s for SMB; the two settings are [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)'s.

| Class | Causes | Held at most until |
| --- | --- | --- |
| `HoldDeadline` | `DelayRecall`, `DelayClone`, a `Delay` with no cause | the cause clears, or `smb_pending_deadline`, which **MUST** exceed the break deadline so the request outlives the recall it waits on |
| `HoldUntil` | `DelayFrozen`, not moved | `Until` plus the break deadline, never past `migration.freeze_timeout` plus the break deadline ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)) |
| `HoldCap` | `DelayConflict`, `DelayCapacity`, `DelayCommit`; and `ErrUnavailable` | the cause clears, or `smb_pending_cap`, which **MUST** exceed `migration.freeze_timeout` plus the break deadline |
| `HoldNone` | `DelayResources`; `DelayFrozen` with `Moved` set | never held: answered at once |
| — | `ErrGrace` | the shard's grace end plus the break deadline ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)) |
| — | `ErrBlocked` | no deadline: it holds no worker and ends only as its waiter does ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)); an adapter that answers it other than as granted calls `CancelLock` first |

A `HoldCap` cause clears by itself, unlike `ErrLost` or `ErrNoSpace`; SMB has no
retry-later status, so answering at the call's deadline would fail every write
a Windows client had in flight for a stall an NFS client rides out by
retrying. A moved share's requests are answered as the `RevokedShare`
revocation that follows says.

**Recalls after one is revoked.** After one of a client's recalls is revoked, the
service **MUST** stop offering that client grants and recall the others it holds
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
An SMB break that times out is forced — the lease downgraded to none — and the
request held behind it proceeds; the break keeps the opens it covered, with
their locks and deny modes, so the holder may keep writing through them; a
revoked NFS delegation's later writes are refused (the same section).

**A share removed or moved.** `DropShare` ends every open, lock, waiting lock,
grant and watch under the share and reports them `RevokedShare`; the adapter
answers what it holds pending on them. What it does with a moved share's
address and the trees reached through other addresses is
[RFC 20 §3.3](rfc-20-adapters.md#3.3%20Callbacks%20are%20the%20adapter%27s%20to%20deliver)'s
and [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)'s.

**A callback path can move between nodes** (cluster, [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)).
A client's callbacks go out through the protocol node that holds its back
channel. When the client binds its callback path through another node — after
an address takeover, or a lost connection — that node calls `Connect` again,
and the front records it in the client's record (RFC 14's `Rebind`); the
latest wins, and a recall in flight is retried through it. Without the record,
every recall after the client moved would go out through a path the client no
longer listens on and end in revocation.

**Notifications honour traverse permission.** `Notify` delivers a change only
when the watch's identity may traverse every directory between the watched one
and the change, evaluated through the chokepoint ([§4.6](#4.6%20One%20chokepoint))
at delivery. A recursive watch never reports names below a directory its holder
cannot enter.

### 3.3 Event hooks

```go
// Events is implemented by an event sink supplied at composition. The
// service calls it and never waits on it.
type Events interface {
	Emit(ctx context.Context, e Event) // a QuotaEvent or an AccessEvent
}

type AccessEvent struct {
	Time     time.Time
	Share    ShareID
	Client   ClientID // zero for NFSv3
	Identity Identity
	Protocol Protocol
	Op       Op
	File     FileID
	Name     []byte // the name acted on, for namespace operations
	Want     Access
	Allowed  bool
}
```

`QuotaEvent` names the quota, the principal or project, and what happened:
advisory threshold crossed, soft limit exceeded, grace expired, hard refusal.
Both kinds are emitted from the one path every operation takes: access events
where the shard consults the chokepoint ([§4.6](#4.6%20One%20chokepoint)), quota
events where it enforces the quota ([§5.6](#5.6%20Quota)). No adapter, store or
engine emits either, so no protocol can reach a file without appearing in the
audit stream.

- An event hook **MUST NOT** block or fail the operation. The sink queues, and
  an event it cannot queue is dropped and counted (§7).
- Which operations produce an access event — every decision, denials only,
  writes and namespace changes, per share — is the audit policy, a setting
  ([RFC 13](rfc-13-configuration.md)). With no policy, no access event is built.
- Delivery, retention and the export format of both streams are
  [RFC 25](rfc-25-observability.md)'s. This RFC fixes only where they are
  emitted and what an event carries.

### 3.4 IDs and their encodings

Every name a client presents back is decoded by a pure function in
`pkg/vfs/ids.go` into the service value it encodes, so both adapters use one
codec and keep no table
([RFC 20 §2.2](rfc-20-adapters.md#2.2%20What%20an%20adapter%20may%20hold)).
Decoding never touches the store. Whether a decoded value is stale or bad is the
service's answer when the value is used — `ErrStale`, or a `StateError` kind —
never the codec's.

```go
func EncodeStateID(ref OpenRef, seq uint32) [16]byte // names indicative
func DecodeStateID(b [16]byte) (OpenRef, uint32, error) // malformed bytes only; never a lookup
func EncodeFileID(ref OpenRef) (persistent uint64)
func DecodeFileID(persistent uint64) (OpenRef, error)
```

| ID | Names | Minted by | Encoding | Stable across restart and failover | Stale vs bad decided by |
| --- | --- | --- | --- | --- | --- |
| `Handle` | a file, or a pseudo directory | the metadata store's handle codec ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)) | opaque: the share and the `FileID`, never a shard, path or name; a pseudo directory's is its own kind, a digest of the installation and the path | yes | the shard's `Resolve`: `ErrStale` ([RFC 7 §6.3](rfc-7-namespace-metadata.md#6.3%20Staleness%20is%20reported%2C%20never%20guessed)) |
| `Cursor` | where a listing resumes | RFC 7's entry digest ([RFC 7 §3.5](rfc-7-namespace-metadata.md#3.5%20A%20cookie%20survives%20concurrent%20mutation)) | 63 bits, top bit clear, 0–2 reserved; the cookie verifier is a constant of the share ([RFC 21 §7.2](rfc-21-nfs.md#7.2%20READDIR%20cookies%20and%20verifier)) | yes | the listing: a resume past a removed entry still resumes |
| NFSv4 stateid | an open, lock state, delegation, layout or copy | RFC 14 ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)) | `seqid` 4 bytes; `other` 12: byte 0 the type tag (low 3 bits) and the grace instance's low 5 bits, bytes 1–3 the shard number, bytes 4–11 the record's 64 random bits ([RFC 21 §5.2](rfc-21-nfs.md#5.2%20Stateids)) | yes; lost state is told by the grace bits | the primary: `StateError` `StaleState` or `BadState` |
| anonymous and special stateids | the anonymous open; the current stateid | the protocol | all zeros, all ones, NFSv4.1's current | — | the adapter resolves "current" within the compound; all ones is anonymous for `READ` only |
| SMB `FileId` | an open | RFC 14's `OpenID` | persistent half: the open's shard and its `OpenID` ([RFC 22 §6.1](rfc-22-smb.md#6.1%20Opening%20a%20file)); volatile half: the protocol node's table entry, the adapter's alone | the persistent half yes; the volatile half no | the primary, for the persistent half; the adapter, for the volatile |
| `ClientID` | a client | RFC 14, from the client record | 8 bytes, unique and never reused, not random ([RFC 21 §5.1](rfc-21-nfs.md#5.1%20Client%20ID%2C%20lease%20time%2C%20expiry)) | yes | the lease owner: `StaleClient` |
| `SessionRef` | an SMB session | the protocol node | (node, node epoch, session ID) ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)) | no: a new session after a restart | — |
| `GrantID`, lease key | a caching grant | RFC 14 / the client (lease key) | RFC 14's ID; an SMB lease is named by its client-chosen 16-byte key | yes | the primary: `BadState` |
| `WatchID`, `LockWaiter` | a watch, a waiting lock | RFC 14 | RFC 14's IDs | no: volatile, lost with the primary | the primary: `BadState` |
| NLM owner, cookie | a lock owner; a reply's match | the client | `(svid, oh)` and the host's `caller_name` plus address ([RFC 21 §6.1](rfc-21-nfs.md#6.1%20NLM)); the cookie is echoed, never decoded | — | — |

> decision: an ID is self-describing wherever the protocol allows it — an
> NFSv4 stateid, whose `other` carries unguessable random bits, a `READDIR`
> cookie, and the persistent half of an SMB `FileId`, since a durable
> reconnect may land on another node — and kept in a table only where the
> protocol scopes it to a session or connection: an SMB tree ID, session ID and
> the `FileId`'s volatile half. One codec per ID, in `pkg/vfs`, shared by both
> adapters; decoding never trusts, and stale against bad is the service's answer
> on use. Rejected: tables for everything, whose state dies with the connection
> while the client still holds the name. Overturned if a protocol is found whose
> client-held name has no room for what decoding needs.

> ponytail: RFC 21 §5.2 gives the stateid's `other` 24 bits of shard number,
> while RFC 22 §6.1 says only that the `FileId`'s persistent half "names the
> open's shard" in 8 bytes beside the `OpenID`. The two need one shard-number
> width and one `OpenID` width; this table does not choose (§10).

### 3.5 Operations, by wire request

Every request both protocols serve, the service method it becomes, and what
that method calls below. "Catalog" means the store-wide reads of
[§2.1](#2.1%20What%20runs%20where), served on any node; "shard" means the file's
primary; "front" means the protocol node routes or coordinates and makes no
store call. Gaps in the layers below are rows of §10's last question.

**By service method.**

| Method | Metadata calls (RFC 7, RFC 6, RFC 14, RFC 16, RFC 12) | Engine calls (RFC 8) | Runs at |
| --- | --- | --- | --- |
| `Connect`, `CreateSession`, `Renew`, `Destroy` | `OpenState.Connect` (and `Rebind`), `CreateSession`, `Renew`, `Destroy` | — | catalog, on the lease owner's node; the front keeps the callback registry |
| `Disconnect`, `ReclaimComplete`, `FreeRevoked`, `HostRestarted` | `OpenState.Disconnect`, `ReclaimComplete`, `FreeRevoked`, `HostRestarted` | — | shard, each primary holding the client's state |
| `TestState`, `ReleaseLockOwner` | gap: RFC 14 declares no call that tests a stateid or drops a lock owner | — | shard |
| `Lookup` | catalog `Namespace.Lookup` (authorised inside, [RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)) | `Overlay` | catalog, then the file's shard for the overlay |
| `ReadDir` | catalog `Namespace.Entries` or `EntriesPlus` | `Overlay` per entry, batched per primary | catalog; front fans the overlay out |
| `Create`, `Link` | `Namespace.Create` / `Link`, `OpenState.CheckChange` on the parent | `Draw` (parent, new file) | shard; front coordinates across shards |
| `Unlink`, `Rmdir`, `Rename` | `Namespace.Lookup`, `Files.Authorize`, `OpenState.CheckChange` on the file, a replaced target and the destination parent, `Namespace.Unlink` / `Rename` | `Draw`; `Release` for an orphan | shard; front coordinates across shards |
| `GetAttr` | `Files.Get` | `Overlay` | shard |
| `SetAttr` | `Files.SetAttrs`, `Files.Authorize`; `OpenState.CheckChange`, `SetDisposition`, `SuspendTimes` | `Draw`; `Truncate` for a size | shard |
| `GetACL`, `Xattrs`, `Streams`, `Access` | catalog `Files.ACL`, `Xattrs`, `Streams`, `Granted` | — | catalog |
| `SetACL`, `SetXattr`, `RemoveXattr` | `Files.SetACL`, `SetXattr`, `RemoveXattr` | `Draw` | shard |
| `Open` | `Namespace.Lookup` or `Create`, `Files.Authorize`, `Granted`, `OpenState.Open` or `Reconnect` | `Draw` when it creates | shard ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| `Downgrade`, `ConfirmOpen` | gap: RFC 14 declares neither | — | shard |
| `Close` | `OpenState.Close`; `Namespace.Unlink` for a delete on close | `Release` for an orphan | shard ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| `Read` | `OpenState.CheckIO`; `Files.Authorize` for an anonymous or delegated open | `Read` | shard |
| `Write` | `OpenState.CheckIO`; `Files.Authorize`; `Files.SetAttrs` to clear setuid; catalog `Capacity.Usage` for quota | `Draw`, `Write`, `Overlay`, `Commit` when stable | shard |
| `Commit` | `OpenState.Flushed` through an SMB open | `Commit` | shard |
| `Deallocate`, `Seek` | `OpenState.CheckIO` | `Deallocate`; `Allocation` | shard |
| `Copy`, `Clone` | `OpenState.CheckIO` on both | `Copy`, `Clone`; across shards `Read` and `Write` | shard; front runs a copy across shards |
| `Lock`, `CancelLock`, `TestLock`, `Unlock` | `OpenState.Lock`, `CancelLock`, `TestLock`, `Unlock` | — | shard ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| `AckBreak` | `OpenState.AckBreak`, or `Return` for `GrantNone` | — | shard |
| `Watch`, `Unwatch` | `OpenState.Watch`, `Unwatch` | — | shard of the directory |
| `Root`, `SecInfo`, `ListShares` | catalog: share records and export policy (gap: RFC 16 declares no read view of the share indexes but `ControlPlane.Shares`), `Files.Root` | — | front, over the catalog |
| `StatFS`, `Quota` | catalog `Capacity.Info`, `Capacity.Usage`; the quota record (gap: no view but `ControlPlane.Quotas`) | — | catalog |
| `Snapshots` | RFC 12 `Snapshots.List`, filtered by the handle's path | — | catalog |
| `Spell`, `Find` | `Principals.ByName`, `ByProtocolID`, through RFC 18's resolution | — | catalog |

**NFSv3 and its auxiliary services.**

| Request | Method | Notes |
| --- | --- | --- |
| `NULL` | none (adapter only) | |
| `GETATTR` | `GetAttr` | |
| `SETATTR` | `SetAttr` | the ctime guard is `AttrChange.Guard`; returns `wcc_data` |
| `LOOKUP` | `Lookup` | `..` at a share root answers the root ([§5.10](#5.10%20Lookup%20and%20create)) |
| `ACCESS` | `Access` | |
| `READLINK` | `GetAttr` | the stored target, `File.Target` |
| `READ` | `Read` on `vfs.Anonymous(h)` | |
| `WRITE` | `Write` on `vfs.Anonymous(h)` | `WriteResult` gives count, verifier and `wcc_data` |
| `CREATE`, `MKDIR`, `SYMLINK`, `MKNOD` | `Create` | `EXCLUSIVE` carries `CreateArgs.Verifier` |
| `REMOVE` | `Unlink` | |
| `RMDIR` | `Rmdir` | |
| `RENAME` | `Rename` | |
| `LINK` | `Link` | |
| `READDIR`, `READDIRPLUS` | `ReadDir` | `want` asks for attributes and handles for `READDIRPLUS` |
| `FSSTAT` | `StatFS` | |
| `FSINFO`, `PATHCONF` | none (adapter only) | constants of the adapter and the share ([RFC 21 §2.3](rfc-21-nfs.md#2.3%20Transfer%20sizes)) |
| `COMMIT` | `Commit` | |
| MOUNT `MNT` | `Root` with the path | |
| MOUNT `EXPORT` | `ListShares` | |
| MOUNT `UMNT`, `UMNTALL`, `DUMP` | none (adapter only) | no mount table ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)) |
| NLM `TEST` | `TestLock` on an anonymous open | |
| NLM `LOCK` | `Lock` | `Wait` for a blocking lock; `Reclaim` in grace ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| NLM `CANCEL` | `CancelLock` | answered only once it returns |
| NLM `UNLOCK` | `Unlock` | |
| NLM `GRANTED` (to the host) | the `LockWaitOver` callback | |
| NLM `SHARE`, `UNSHARE` | `Open`, `Close` of an anonymous-owner open with a deny mode | |
| NLM `FREE_ALL` | `HostRestarted` | from the host's own address only |
| NLM `*_MSG`, `*_RES` | as their synchronous forms | answered on the host's callback service |
| NSM `SM_NOTIFY` (received) | `HostRestarted` | |
| NSM `SM_MON`, `SM_UNMON` | `Connect` (the host's monitor name) | |
| rquota `GETQUOTA` | `Quota` | |

**NFSv4.0, 4.1 and 4.2.** `COMPOUND` runs its operations as sequences of these.

| Operation | Method | Notes |
| --- | --- | --- |
| `SETCLIENTID`, `EXCHANGE_ID` | none (adapter only) | an unconfirmed proposal, no state ([RFC 21 §3.5](rfc-21-nfs.md#3.5%20Confirming%20a%20client)) |
| `SETCLIENTID_CONFIRM` | `Connect` | |
| `CREATE_SESSION` | `Connect` once, then `CreateSession` | the replay is the client record's, shared across nodes (§4.2) |
| `SEQUENCE` | `Renew`, at most once per renewal interval | the slot table is the adapter's |
| `RENEW` | `Renew` | |
| `DESTROY_SESSION` | none (adapter only) | |
| `DESTROY_CLIENTID` | `Destroy` | |
| `BIND_CONN_TO_SESSION`, `BACKCHANNEL_CTL` | `Connect` when the back channel moves | |
| `RECLAIM_COMPLETE` | `ReclaimComplete` | |
| `PUTROOTFH`, `PUTPUBFH` | `Root` with the zero `ShareRef` | |
| `PUTFH`, `GETFH`, `SAVEFH`, `RESTOREFH` | none (adapter only) | the next operation's call admits the handle |
| `LOOKUP`, `LOOKUPP` | `Lookup` (`..` for `LOOKUPP`) | |
| `GETATTR`, `VERIFY`, `NVERIFY` | `GetAttr` (and `GetACL` for the `acl` attribute) | the comparison is the adapter's |
| `SETATTR` | `SetAttr` (and `SetACL` for `acl`) | |
| `ACCESS` | `Access` | |
| `CREATE` | `Create` | |
| `LINK`, `REMOVE`, `RENAME` | `Link`, `Unlink`, `Rename` | `change_info4` from the `WCC` |
| `READDIR` | `ReadDir` | |
| `READLINK` | `GetAttr` | |
| `OPEN` | `Open` | create, claim types and reclaim are `OpenRequest` fields ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| `OPEN_CONFIRM` | `ConfirmOpen` | NFSv4.0 |
| `OPEN_DOWNGRADE` | `Downgrade` | |
| `CLOSE` | `Close` | |
| `READ`, `WRITE`, `COMMIT` | `Read`, `Write`, `Commit` | |
| `LOCK`, `LOCKT`, `LOCKU` | `Lock`, `TestLock`, `Unlock` | ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| `RELEASE_LOCKOWNER` | `ReleaseLockOwner` | |
| `DELEGRETURN` | `AckBreak` to `GrantNone` | |
| `DELEGPURGE`, `WANT_DELEGATION`, `GET_DIR_DELEGATION`, `OPENATTR`, `SET_SSV` | not supported, as [RFC 21](rfc-21-nfs.md) answers each | |
| `TEST_STATEID` | `TestState` | |
| `FREE_STATEID` | `FreeRevoked` | a lock state holding no locks: `ReleaseLockOwner` |
| `SECINFO`, `SECINFO_NO_NAME` | `SecInfo` | |
| `ALLOCATE` | not supported ([§3.1](#3.1%20Operations)) | |
| `DEALLOCATE`, `SEEK` | `Deallocate`, `Seek` | |
| `READ_PLUS` | `Read` and `Seek` | |
| `COPY` | `Copy` | always answered synchronously, short at the deadline ([RFC 21 §9.4](rfc-21-nfs.md#9.4%20Async%20COPY)) |
| `OFFLOAD_STATUS`, `OFFLOAD_CANCEL` | none (adapter only) | no copy stateid is ever issued; answered as one naming nothing |
| `CLONE` | `Clone` | |
| `GETXATTR`, `LISTXATTRS`, `SETXATTR`, `REMOVEXATTR` | `Xattrs`, `SetXattr`, `RemoveXattr` | |
| `COPY_NOTIFY`, `IO_ADVISE`, `WRITE_SAME`, `LAYOUT*`, `GETDEVICE*` | not supported ([RFC 21 Appendix C](rfc-21-nfs.md#Appendix%20C%20%E2%80%94%20NFSv4.2%20operations)) | |

**SMB 2 and 3.**

| Command | Method | Notes |
| --- | --- | --- |
| `NEGOTIATE` | `Connect` | |
| `SESSION_SETUP` | none (adapter and RFC 18's authenticator) | |
| `LOGOFF` | `Disconnect` | |
| `TREE_CONNECT` | `Root` with the name; `\IPC$`: none | `IPC$` is the adapter's RPC endpoint, not a share, and reaches no file |
| `TREE_DISCONNECT` | none (adapter only) | |
| `CREATE` | `Open` | durable and reconnect contexts (`DHnQ`, `DH2Q`, `DHnC`, `DH2C`), `RqLs`, `MxAc`, `QFid`, `SecD`, `ExtA`, `AlSi`, `TWrp` and `AppInstanceId` are `OpenRequest` fields ([RFC 22 Appendix D](rfc-22-smb.md#Appendix%20D%20%E2%80%94%20create%20options%20and%20contexts)); `FILE_OPEN_BY_FILE_ID` and shared virtual disks not supported |
| `CLOSE` | `Close` | |
| `FLUSH` | `Commit` | |
| `READ`, `WRITE` | `Read`, `Write` | |
| `LOCK` | `Lock` or `Unlock`, every element in one call | ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| `IOCTL` `SRV_COPYCHUNK`, `_WRITE` | `Copy` | `SRV_REQUEST_RESUME_KEY`: none (adapter only) |
| `IOCTL` `DUPLICATE_EXTENTS_TO_FILE` | `Clone` | |
| `IOCTL` `SET_ZERO_DATA` | `Deallocate` | |
| `IOCTL` `QUERY_ALLOCATED_RANGES` | `Seek` | |
| `IOCTL` `SET_SPARSE` | `SetAttr` | |
| `IOCTL` `GET_REPARSE_POINT` | `GetAttr` | |
| `IOCTL` `SET_REPARSE_POINT`, `DELETE_REPARSE_POINT` | `Create` of a symlink; `Unlink` | gap: RFC 7 has no call that turns an existing empty file into a symlink |
| `IOCTL` `GET_OBJECT_ID`, `CREATE_OR_GET_OBJECT_ID` | `GetAttr` | derived ids |
| `IOCTL` `SRV_ENUMERATE_SNAPSHOTS` | `Snapshots` | |
| `IOCTL` `QUERY_NETWORK_INTERFACE_INFO`, `VALIDATE_NEGOTIATE_INFO`, `PIPE_*` | none (adapter only) | |
| `IOCTL` ODX, integrity, compression, `SVHDX_*`, resiliency | not supported ([RFC 22 Appendix F](rfc-22-smb.md#Appendix%20F%20%E2%80%94%20control%20codes)) | |
| `QUERY_DIRECTORY` | `ReadDir` | |
| `CHANGE_NOTIFY` | `Watch` | |
| `QUERY_INFO` file classes | `GetAttr`; `Streams` for `Stream`; `Xattrs` for `Ea`, `FullEa`; `Access` for `Access` | |
| `QUERY_INFO` file system classes | `StatFS` | |
| `QUERY_INFO` security | `GetACL` | |
| `QUERY_INFO` quota | `Quota` | |
| `SET_INFO` `Basic`, `EndOfFile`, `Allocation`, `Mode` | `SetAttr` | timestamp suspension is `AttrChange.Suspend` |
| `SET_INFO` `Disposition`, `DispositionEx` | `SetAttr` with `DeleteOnClose` through the open | |
| `SET_INFO` `Rename` | `Rename` with the open as `from.Open` | |
| `SET_INFO` `Link` | `Link` | |
| `SET_INFO` `FullEa` | `SetXattr`, `RemoveXattr` | |
| `SET_INFO` security | `SetACL` | |
| `OPLOCK_BREAK` (lease or oplock acknowledgement) | `AckBreak` | |
| `CANCEL` | `CancelLock` for a waiting lock, `Unwatch` for a notification; otherwise none | |
| `ECHO` | none (adapter only) | |
| srvsvc `NetShareEnumAll` | `ListShares` | |
| srvsvc `NetShareGetInfo` | `ListShares`, filtered by name | |
| srvsvc `NetSrvGetInfo`, wkssvc `NetWkstaGetInfo` | none (adapter only) | |
| lsarpc `LookupSids2/3`, `LookupNames3/4` | `Spell`, `Find` | |

## 4. Rules

### 4.1 The operation set is the union, not the intersection

`Open` takes a create disposition, requested access, a deny mode, a wanted
caching grant and a durability, because SMB needs create-open-deny as one
atomic step and NFSv4's `OPEN` has the same shape. NFSv3 has no opens: its reads
and writes run against an **anonymous open**, which carries no state, and the
call's own identity authorises them. An operation one protocol needs and the
other lacks — `Copy`, `Seek`, `Watch`, streams — is in the set; an adapter whose
protocol lacks it simply does not call it.

A protocol's semantics **MUST NOT** be approximated by composing weaker service
calls in the adapter. If SMB's atomic create-with-deny were built from a lookup,
a create and an open, two clients could interleave between them. The service
offers the atomic form, or the operation is not supported.

### 4.2 Translation stays in adapters

Which logic is the adapter's and which the service's is decided by
[RFC 20 §2.1](rfc-20-adapters.md#2.1%20Two%20questions%20place%20every%20piece%20of%20logic)'s
two questions, and what an adapter may hold, import and keep is
[RFC 20 §2.2–§2.4](rfc-20-adapters.md#2.2%20What%20an%20adapter%20may%20hold).
One placement this RFC adds: the NFSv4.1 `CREATE_SESSION` replay is the
service's, through `CreateSession`, unlike a session's reply cache, because the
client record it belongs to is shared across protocol nodes
([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)).

### 4.3 Errors are neutral values

The service returns the errors below, declared in `pkg/vfs/errors.go`. The
content errors are declared once in the errors package the engine and the
service share, and re-exported as aliases, so the engine returns them without
importing the layer above. `vfs.Errors()` returns every sentinel and one value
of every typed kind, so an adapter's error table can be checked total
([RFC 20 §7.3](rfc-20-adapters.md#7.3%20The%20error%20tables%20are%20total)).
How each reaches the wire is
[RFC 21 §5.6](rfc-21-nfs.md#5.6%20Open-state%20and%20capacity%20errors%20on%20the%20wire),
[RFC 22 §6.6](rfc-22-smb.md#6.6%20Service%20errors%20on%20the%20wire) and the
adapters' tables ([RFC 20 §4.5](rfc-20-adapters.md#4.5%20One%20error%20table%20per%20protocol)).

| Error | Means | Kind |
| --- | --- | --- |
| `ErrNotFound`, `ErrExist`, `ErrNotEmpty`, `ErrNotDir`, `ErrIsDir` | the name or entry is not as the call needs | — |
| `ErrAccess` | the share grant, its client rules, or the file's mode or ACL refuse the caller | — |
| `ErrPerm` | refused whatever the caller's access: an immutable or append-only file ([RFC 7 §7.8](rfc-7-namespace-metadata.md#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint)), or a change of owner, group or flags only a privileged caller or the owner may make ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)) | — |
| `ErrStale` | a handle to nothing, or to a share being removed or retired ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)) | — |
| `ErrInvalid` | the arguments cannot be honoured as given: an overlapping copy or clone within one file, a clone longer than `clone_max_len`, a deny entry naming a well-known principal [RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)'s table lacks | — |
| `ErrNoSpace` | a permanent capacity refusal: no space comes without operator action ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s second row); answered at once | — |
| `ErrQuota` | a logical quota refused; answered at once | `*QuotaError{Scope}`: `QuotaShare`, `QuotaProject`, `QuotaUser`, `QuotaGroup`, the first exceeded in that order |
| `ErrLocked` | a byte-range lock others hold conflicts | — |
| `ErrRangeNotLocked` | an `Unlock` named a range the owner does not hold | — |
| `ErrShareViolation` | a deny mode others hold conflicts, decided after handle caching is broken ([§5.3](#5.3%20Open%20and%20close)) | — |
| `ErrDeletePending` | the file is marked delete on close ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)) | — |
| `ErrGrace` | the shard is in grace and the request needs, or conflicts with, state that may still be reclaimed | — |
| `ErrDelay` | retry shortly | `*Delay{Cause; Until; Moved; Addr}`, below |
| `ErrBlocked` | a lock waits, and will be granted through `LockWaitOver` | — |
| `ErrWrongSecurity` | the share's policy does not admit the call's flavour ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)) | — |
| `ErrState` | a client-held name of state cannot be used as given | `*StateError{Kind}`, below |
| `ErrRevoked` | the call names state in its client's revoked set, not yet freed | a `*StateError` of kind `Revoked`, `Admin` telling an administrator's revoke from a recall missed |
| `ErrCross` | the call spans what cannot be spanned | `*CrossError{Kind}`: `CrossNamespace` (a copy or clone between namespaces), `CrossShard` (**(cluster)** a clone between shards, [§5.9](#5.9%20Copy%20and%20clone)), `CrossProject` (a rename or link across tree quotas, [RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)) |
| `ErrIncomplete` | a read of a file a snapshot lists as incomplete, or a copy or clone from one ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) | — |
| `ErrInconsistent` | a store invariant the server's own state breaks; the store raises a health condition naming the record ([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)) | — |
| `ErrLost`, `ErrUnavailable`, `ErrCorrupt` | the content errors of [RFC 8](rfc-8-engine.md) | aliases |

`StateError` kinds, each matched by `errors.Is(err, ErrState)`, are RFC 14's
refusals: `ClientInUse` (an NFSv4 owner another principal holds), `ClientBusy`
(`Destroy` with state left), `SeqMisordered` (a create-session sequence out of
order), `StaleClient` (a client that expired, or is marked by a shard's loss
until it repeats `SETCLIENTID`, [RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)),
`StaleState` (a stateid or `FileId` whose state may have been lost with its
shard's open state, by the grace-instance rule of §3.4), `BadState` (every
other stateid or `FileId` whose record is missing — expired, freed, a removed or
moved share's, never issued), `NoReclaim`, `ReclaimRevoked` (an NFSv4 reclaim
in a shard where an administrator revoked the client's state), `BadSeqID` (an
NFSv4.0 owner sequence out of order), `NotYours` (state another client or
principal holds, §3.1) and `Revoked`.

```go
type Delay struct {
	Cause DelayCause
	Until time.Time  // DelayFrozen: the freeze's bound; zero otherwise
	Moved bool       // DelayFrozen: the share's namespace has moved away
	Addr  netip.Addr // Moved: the address the move names, or zero
}

func (d *Delay) Hold() HoldClass // §3.2's table

type DelayCause uint8

const (
	DelayRecall    DelayCause = iota // a caching grant is being recalled (§3.2)
	DelayClone                       // a running clone or copy chunk holds the extent past the call's hold (§5.9)
	DelayConflict                    // a metadata conflict still unresolved at the call's deadline (RFC 0 §9.2)
	DelayFrozen                      // the share is quiesced, frozen for a move, or moved away
	DelayCapacity                    // the journal's capacity or a share's journal limit (RFC 8 §10.2)
	DelayCommit                      // a metadata commit is not landing: a stalled store, the version limit's raise, or quota slack spent (RFC 8 §11.2, §5.6)
	DelayResources                   // a node memory pool is spent: the share cap or the open-state pool (RFC 24)
)
```

`Cause` labels metrics and selects the final status of a held request; the hold
itself is `Hold()`'s. **A transient refusal never becomes `ErrNoSpace` because a
deadline ran out**: a capacity refusal still `ErrDelay` at the caller's
deadline is answered `ErrDelay`, and only [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s
permanent causes are `ErrNoSpace`. A conflict the shard still meets at the
call's deadline, its retries spent, is `DelayConflict`, never an I/O error
([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)).
A routing refusal — wrong primary, stale epoch — **MUST NOT** reach an adapter:
the front re-routes and retries within the caller's deadline
([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20primaries)).

> decision: one hold class per cause, returned by `Hold()`, and no second
> classification. A2's grouping of causes into contention, freeze, capacity,
> stall and resources differs from the hold classes only in the final status,
> which is the protocol RFC's per cause. Add a class only when a cause needs a
> hold bound no class gives.

**Every call carries its deadline; the service sets none.** The adapter's
pipeline gives each attempt a fixed 30 s from its arrival, carried in the
context ([RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits)). A
request an adapter holds is bounded in total by its class (§3.2) —
`smb_pending_deadline`, 60 s, on a recall, `smb_pending_cap` for a transient
refusal, none for a waiting lock and `CHANGE_NOTIFY` — and each re-drive is a
fresh 30 s attempt inside that bound; a drain or a closed connection cancels it.
So every wait below the service ends
([RFC 0 §10.3](rfc-0-data-lifecycle.md#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)).

> decision: the attempt deadline is fixed at 30 s, set by the pipeline, not
> measured and not a setting. Every internal wait is sized below it, so a
> setting would let an operator break bounds other RFCs derive from it (the
> single-node self-fence of
> [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) is
> one). Rejected: the service setting it on entry, as today's 30 s at the
> block-store calls does as a workaround for requests that carry none; and a
> setting per protocol. Revisit it if a measured operation class completes
> correctly but past 30 s under load, or if a client is shown to give up on a
> call sooner.

### 4.4 Handles are opaque

An adapter stores and returns handles, and **MUST NOT** build, parse or compare
their contents ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path),
§3.4).

### 4.5 No copies on the data path

`Read` fills the caller's buffer and `Write` takes the caller's; the service
**MUST NOT** add a copy of its own. The rest is
[RFC 20 §5.3](rfc-20-adapters.md#5.3%20No%20copies%20on%20the%20data%20path)'s
and [RFC 24](rfc-24-resources.md)'s.

### 4.6 One chokepoint

Authorisation stays in the metadata store's `Files.Authorize`
([RFC 7 §7.1](rfc-7-namespace-metadata.md#7.1%20One%20chokepoint)). The shard
package's `authorize.go` calls it on every operation that needs it, and is its
only caller. An open-time grant ([RFC 7 §7.2](rfc-7-namespace-metadata.md#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always))
is computed there and stored with the open; the shard evaluates it on the
operations it gates. Every call runs admission, then authorisation, then the
conflict check, in that order:

```go
// The order of every call (indicative). Admission is the front's (§4.9), the
// rest the shard's; RFC 7 §7 owns what Authorize decides.
caller, err := front.admit(ctx, id, shareOf(h)) // state, flavour, client rules, squash → RFC 18's Caller
s := front.route(ctx, h)                         // the file's primary
// at the shard:
switch {
case t.Open != nil && t.Open.Kind == Opened: // through an open: its stored grant
	g := openGrant(t.Open)                     // RFC 14's Access, Privileged and Via
	if !samePrincipal(caller, g) { return StateError{NotYours} }
	err = meta.Files.Authorize(ctx, caller.With(g), h, g.Via, want)
default: // anonymous, delegated or no open: the caller afresh
	err = meta.Files.Authorize(ctx, caller, h, parent, want) // parent: the directory h was resolved through, or zero
}
events.Emit(ctx, AccessEvent{ /* … */ }) // if the audit policy selects it
err = open.CheckIO(ctx, ref, r, write)  // or CheckChange: deny modes, locks, grants — RFC 14 §7
```

- **The share grant is checked on every call**, not only at `Root`.
  `Authorize` evaluates the caller's `ShareGrant` each time, and any cache of
  authorisation results **MUST** include the grant, and the parent directory
  with its whole record's version — owner and group as well as mode and ACL,
  since the group bits and the sticky rule read them — in its key
  ([RFC 7 §7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read)).
  A client removed from a share loses access on its next call, cached handles
  or not.
- **An open is decided with its parent.** The shard passes `Authorize` and
  `Granted` the directory an open, an unlink, a rename or an `Access` resolved
  through as `via` ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)),
  so an SMB open asking for `DELETE`, or for `MAXIMUM_ALLOWED`, is granted it
  when the parent grants deleting a child, as removing the name over NFS is.
  Holding a handle grants nothing: a handle names a file, and every call on it
  is authorised afresh.
- **A call through an open uses the open's grant.** A write, and a
  set-information, rename, delete, delete on close, set-security or EA change
  whose `Target.Open` is set, pass the metadata store an `OpenGrant` built from
  that open's record — its file, the access granted at open, whether it is
  privileged (an SMB open with backup or restore intent by a caller privileged
  for it) and the directory it resolved through
  ([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface),
  [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)) — and are evaluated against
  it, not re-checked against the directory or the file afresh, so a rename
  through a handle opened with `DELETE` needs no new delete-child decision. A
  privileged grant makes every request through it privileged — owner, group
  and DACL set included — as [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)
  states; `MaximalAccess` is still computed without the privilege.
- **Deny modes.** A granted open's deny mode is evaluated once, at open
  ([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)).
  An anonymous open has no open to evaluate at, so each of its reads and writes
  is checked against the deny modes held by others: a read under deny-read and
  a write under deny-write are refused with `ErrShareViolation` — after the
  handle-caching grants covering the conflicting opens are broken (§5.3).

### 4.7 Callable across the network

A call may reach a node that does not own the file. Every `Shard` and `Catalog`
method therefore takes and returns values and resumes a listing from a cursor
the caller holds, and every forwarded call carries
[RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)'s route
envelope, whose dedup table returns a mutation's original result to a retry.
Repeating a mutation with the same arguments is **not** assumed to be safe.

`ReadDir` returns one page and the cursor after it; the front fills it by asking
the catalog for entries after the caller's cursor, so no iterator state lives
on any node between calls. An adapter fits the page to its reply
([RFC 20 §4.9](rfc-20-adapters.md#4.9%20Sharing%20code%20across%20versions%20and%20protocols))
and resumes from the cursor of the last entry it sent.

### 4.8 One primary per file

Every call that changes a file, or checks open state on it, runs at the primary
of the file's shard ([RFC 11](rfc-11-ownership.md)), which holds the file's
namespace records, its open state and its content. The front only forwards. The
conflict check and the I/O it gates therefore run at one primary, with no hop
between them. The primary **MUST** order them so that no lock, deny mode or
grant is granted between the check and the I/O it admits: a conflicting grant
is ordered wholly before the check, which then refuses, or wholly after the I/O.

The shard does no cross-primary orchestration for a single-file operation. The
operations that touch two primaries are those
[RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries)
and [RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards) name —
a rename, link or unlink whose files are in several shards, and a create whose
new file lands in a shard other than its directory's, which draws the new
file's version at the new file's primary (§5.10) — which the front orders
through one coordinator, and `ReadDir` with attributes, which reads each
entry's overlay at its own primary ([§5.7](#5.7%20GetAttr)) and needs no order
between them. A `Copy` between files of two shards is run by the front as reads
at one primary and writes at the other, and a `Clone` between them is refused
with `CrossShard` ([§5.9](#5.9%20Copy%20and%20clone),
[RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)).

### 4.9 Shares, mounts and trees

**Admission runs on every call.** Before anything else, the front admits the
call to the share of the handle or open it names, in this order:

1. the share's state ([RFC 16 §2.3.1](rfc-16-metadata-store.md#2.3.1%20Share%20names%2C%20paths%20and%20state)) — `ErrDelay` (`DelayFrozen`) while quiesced,
   `ErrStale` once being removed or retired ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share));
2. the share's `ExportPolicy` flavour rule — `ErrWrongSecurity`;
3. its client rules, against the call's address and the netgroups it is in —
   `ErrAccess`, or a narrowing to read-only;
4. resolution, which turns the `Identity` into the caller it acts as on this
   share, squashing included ([RFC 18 §6.1](rfc-18-identity.md#6.1%20From%20Identity%20to%20caller)).

Then the shard's `Authorize` evaluates the grant and the file (§4.6). **There is
no call that skips admission**: lock, unlock, test-lock, watch, grant
acknowledgement and the calls that name only an open are admitted like a read.
Admission is a property of the share a call reaches, not of how it reached it,
so an NFSv4 compound that `PUTFH`s a handle and one that walks the
pseudo-filesystem into the share meet the same four checks. A compound crossing
from one share into another is re-admitted, and re-resolved, at the crossing.
The admission result may be cached per (share, identity) only with
`ExportPolicy.Version`, the netgroups' versions and `ShareList.Version` in the
key.

![Admission on every call: a PUTFH of a held handle, a walk of the pseudo-filesystem from PUTROOTFH, and a call naming only an open all reach the share of the handle or open they name and meet the same four checks — share state (ErrDelay while quiesced, ErrStale once being removed or retired), the flavour rule (ErrWrongSecurity), client rules (ErrAccess or a narrowing to read-only) and squash — then Authorize; a compound crossing shares is re-admitted, and a cached admission result is keyed with the policy, netgroup and share-list versions](img/rfc17-admission.svg)

**A share's removal joins the calls admitted before it.** The shard counts, per
share at each primary, the calls admitted to it and not yet answered. Once
admission refuses the share as being removed, the removal's step 3
([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)) waits
for that count to reach zero before it ends the share's open state, alongside
the engine's own join ([RFC 8 §2.3](rfc-8-engine.md#2.3%20One%20engine%20per%20node%3B%20a%20share%20is%20a%20context)):
a create, link or rename enters the engine only to draw its versions, which
counts nothing, so only this count holds one admitted before the mark from
committing a record after the removal has passed it.

**The service owns every step of reaching a share; the adapter only
translates.** What the protocols share — resolving a name or path, admission,
the pseudo-filesystem, what `SECINFO` answers and which shares a client may
list — is written once, in the front. `Root` resolves a share by the path or
name the client sent, admits, authorises the share grant, and returns the
share's root handle and `ShareInfo`; the zero `ShareRef` returns the
pseudo-root, from which an NFSv4 client `Lookup`s. NFSv3 `MNT`, NFSv4
`PUTROOTFH` and `PUTPUBFH`, and SMB `TREE_CONNECT` are each one `Root` call.
They are where a client fails *early*; they are not where it is gated, since
every later call is admitted again. An adapter holds no share list, and a
change to the share list reaches every protocol at once.

> decision: share resolution and the pseudo-filesystem live in the service, not
> the adapters, because the rules for reaching a share drifted when each
> adapter had its own copy: a check present on one entry path was missing on
> the other. Move a piece back to an adapter only if a protocol needs a rule the
> others must not share, and then state it in that protocol's RFC.

**Neither is stored.** No record of a mount, a tree connect or an SMB session
exists in the metadata store: the NFSv3 MOUNT table is not kept — `DUMP`
returns an empty list and `UMNT` and `UMNTALL` change nothing, which every
client tolerates; SMB sessions, tree connects and their IDs live in the
protocol node's memory, and a client that reconnects sets them up again before
it reclaims durable or persistent opens, a reclaim refused unless the
reclaiming tree connect's `ShareID` is the open's
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens));
`ClientID` is the only client state that is stored, and it is RFC 14's.

**The NFSv4 pseudo-filesystem is a function of the share list.** The front on
every protocol node builds it from the share path index, and must build the
same one, because a client moved between nodes presents the handles it already
holds. A pseudo directory lists only the components that lead to a share the
caller would be admitted to. Its handles, numeric ids, `fsid`, change attribute
and mount crossings are [RFC 21 §7.4](rfc-21-nfs.md#7.4%20Pseudo-filesystem%2C%20SECINFO%20and%20locations)'s;
the service computes them and returns them in the `File` it answers with.

**Share enumeration** — SMB `NetShareEnumAll` and `NetShareGetInfo`, NFS
`showmount -e` — is `ListShares`: the shares whose policy is not hidden and to
which the caller would be admitted; `NetShareGetInfo` is that list filtered by
name. `IPC$` is the SMB adapter's RPC endpoint, not a share: a tree connect to it
makes no service call, and it reaches no file.

> decision: the MOUNT table is not kept, so `showmount -a` lists nothing. It is
> advisory in the protocol and no client depends on it; keep one only if an
> operator workflow is shown to need it, and then per node and labelled as such.

## 5. Orchestration

The shard is the only code that orders a client operation across components.
Each order below is normative; the pseudocode beside it is indicative, and cites
the RFC that owns each step rather than restating it.

### 5.0 One lock order

**One lock order, for every call.** A call takes what it needs in this order,
and never the reverse: first the open-state serialisation of every file it
touches — the step in which open state checks and admits it (§4.8,
[RFC 14 §11](rfc-14-open-state.md#11.%20Interface)) — in file-ID order; then fold
admission at every directory whose deltas it writes, and at a directory whose
own attributes it changes, that a rename moves, or whose `Subdirs` it recounts,
which run behind fold admission as a fold does; fold admission at several
directories is taken in file-ID order within the call's deadline, and a call
that cannot take them all by then answers `ErrDelay` with `DelayConflict`, so
no cycle forms ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps),
[§9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward));
then the accept locks of every file it draws for, in file-ID order. A call
queued for fold admission holds no accept lock, and one waiting for an accept
lock holds every open-state step it needs already. So a write, which holds its
file's open-state step when it takes the accept lock, and an unlink of that
file's last name, which needs the same open-state step in one step with its
commit, never each hold what the other waits for.
[RFC 7](rfc-7-namespace-metadata.md), [RFC 8](rfc-8-engine.md) and
[RFC 14](rfc-14-open-state.md) cite this order rather than restate it.

**A held call holds nothing.** A call the shard holds behind a clone or a copy's
chunk (§5.9) releases its accept lock, every open-state step it took and its
quota reservation for the length of the hold, and when the hold ends re-runs
§5.1's steps 3–6 from the start, re-checking grace, open state and step 5's
flags as if it had just arrived. Holding them, a write held in a cloning source
extent would keep the source's open-state step while the clone's last batch
waits on that step to delete its clone-kind open; the two would wait on each
other until the write's deadline, and every open, close, lock and break
acknowledgement on the file with them, long enough for a lease break to run out
and be revoked.

### 5.1 Write

| Step | What | Owner |
| --- | --- | --- |
| 1 | resolve the handle, refuse a stale one; admit the call to its share; route to the primary | §4.9, [RFC 15 §6](rfc-15-topology.md#6.%20Learning%20primaries) |
| 2 | through an open: `NotYours` for a caller that is not its principal, then the open's stored grant; through a delegation: `NotYours` for a client that does not hold it; anonymous or delegated: authorise the caller against the file and check deny modes others hold, breaking handle caching before refusing | §3.1, [§4.6](#4.6%20One%20chokepoint), §5.3 |
| 3 | in grace, `ErrGrace` for a write overlapping a lock, deny mode or grant that may still be reclaimed; otherwise the caller's open and others' byte-range locks; a conflicting caching grant starts a recall and returns `ErrDelay` | [RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard), [§7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols) |
| 4 | reserve the charge the write adds, if it extends the file | §5.6 |
| 5 | `ErrPerm` for an immutable file, or an append-only one below its size, releasing step 4's reservation, before anything changes; then, for a caller not privileged — the owner included — on a setuid file, or setgid with group execute, clear those bits in a committed mode change. The same step runs for a truncate, a deallocate and a copy or clone's destination | [RFC 7 §7.8](rfc-7-namespace-metadata.md#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint), [§9.6](rfc-7-namespace-metadata.md#9.6%20setuid%20and%20setgid%20are%20cleared%20when%20an%20unprivileged%20caller%20changes%20the%20file) |
| 6 | under the file's accept lock: check step 5's flags again against the overlay's size; capture `WCC.Before`; the engine's `Write`, passing the times the open has suspended; `WCC.After`; release the lock; for a stable write, the engine's `Commit` over the extent. Every other change of a file draws its version from `Draw` under the same lock; a change refused `ErrVersionBehind` is redrawn and retried within the deadline, never surfaced, and `DelayConflict` at it | [RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more), [RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps), [§9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward), [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open) |
| 7 | reply: a stable write once the journal has synced it **and**, if it overwrites committed content, its existence commit has landed; a stable append or hole fill waits only for the sync. The group commit records existence, usage and `mtime`, and releases the reservation | [RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal) |

Steps 3–6 run at the primary as one step (§4.8), except across a hold (§5.0).
The lock never spans a sync or a group commit.

```go
func (l *Local) Write(ctx context.Context, c Caller, ref OpenRef, off int64, src []byte, st Stability) (WriteResult, error) {
	f, err := l.resolve(ctx, ref.Handle)                 // step 1: ErrStale (RFC 7 §6.3)
	unlock := l.order.OpenState(f)                       // §5.0
	defer unlock()
	if err := l.authorizeIO(ctx, c, ref, f, Write); err != nil { // step 2: §4.6
		return WriteResult{}, err
	}
	if err := l.open.CheckIO(ctx, ref, span(off, src), true); err != nil { // step 3: ErrGrace, ErrLocked, ErrShareViolation, ErrDelay(DelayRecall)
		return WriteResult{}, err
	}
	res, err := l.quota.Reserve(ctx, c, f, growth(f, off, src)) // step 4: §5.6
	if err != nil {
		return WriteResult{}, err
	}
	defer res.ReleaseUnlessCommitted()
	if err := l.flagsAndSetuid(ctx, c, f, off); err != nil { // step 5: ErrPerm; setuid cleared, committed
		return WriteResult{}, err
	}
	if err := l.waitClone(ctx, f, span(off, src)); err != nil { // §5.9: at most WithHold's max, then DelayClone
		return WriteResult{}, err
	}
	var wr WriteResult
	l.order.Accept(f, func() { // step 6, under the accept lock
		wr.WCC.Before = l.captureBefore(ctx, f)              // nil where it cannot be taken (§5.7)
		wr.Verifier, err = l.engine.Write(ctx, f, off, bytes.NewReader(src), int64(len(src)), opts(ref, st))
		wr.WCC.After = l.overlaid(ctx, f)                    // same hold, once staged
	})
	if err == nil && stable(ref, st) {
		_, _, err = l.engine.Commit(ctx, f, off, int64(len(src))) // step 7: RFC 8 §5.1
	}
	wr.N = len(src)
	return wr, err
}
```

**Which writes are stable.** A write is stable when the client asks for it (NFS
`FILE_SYNC` or `DATA_SYNC`, SMB write-through), and every write through a
persistent open, or through any open on a continuously available share, is
stable whatever the client asked: those opens survive a restart, and an
acknowledged write they carry must survive it too. A durable open that is not
persistent is not reclaimable across a restart of the primary
([RFC 14](rfc-14-open-state.md)), so its writes need not be.

A write the journal cannot take — the share's journal limit or the journal's
capacity — returns `ErrDelay` (`DelayCapacity`), not `ErrNoSpace`, while offload
or repack can free space, and stays `ErrDelay` when the caller's deadline
passes. `ErrNoSpace` is answered at once for a permanent cause alone
([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)),
and a logical quota refuses at once (§5.6).

### 5.2 Read

Steps 1–3 as for a write, with read access; then the engine's `Read`, which
resolves residency and streams verified chunks
([RFC 8 §7.2](rfc-8-engine.md#7.2%20The%20reply%20streams%2C%20one%20verified%20chunk%20at%20a%20time)).
It never returns an unverified byte, but it can return `n` verified bytes and
then an error (`ErrLost`, `ErrUnavailable`, `ErrCorrupt`) for the chunk after
them. An adapter answers a short read of the `n` bytes when `n > 0` and its
protocol allows, and the error otherwise; `ErrUnavailable` is transient
(`HoldCap`, §3.2). The wire statuses are
[RFC 21 §5.6](rfc-21-nfs.md#5.6%20Open-state%20and%20capacity%20errors%20on%20the%20wire)'s
and [RFC 22 §6.6](rfc-22-smb.md#6.6%20Service%20errors%20on%20the%20wire)'s. A
reply is never padded with zeros for the bytes that failed.

`SEEK_DATA`, `SEEK_HOLE` and the allocated ranges of `READ_PLUS` and SMB's
allocated-range query come from the engine's `Allocation`
([RFC 8 §7.6](rfc-8-engine.md#7.6%20Allocation%20answers%20from%20the%20hole%20set)),
which counts a zero ref under a newer overwrite record as data; no adapter
derives them from reads.

### 5.3 Open and close

`Open` resolves or creates the name, authorises, checks deny modes against the
opens already held, offers a caching grant if no other client conflicts, and
records the open — all at the file's primary, in one step as the protocol sees
it. `Close` drops the open after checking that the named client holds it.

```go
func (l *Local) Open(ctx context.Context, c Caller, r OpenRequest) (OpenResult, error) {
	if r.Reconnect != nil { // SMB DHnC, DH2C: RFC 14 §8.1, matched on client, principal, create GUID, lease key
		return l.reconnect(ctx, c, r)
	}
	unlock := l.order.OpenState(r.Parent) // §5.0; the file's step joins once it is known
	defer unlock()
	if r.MayCreate() { // §5.4: a delete-pending directory takes no entries
		if err := l.open.CheckChange(ctx, fileOf(r.Parent), c.Client, ChangeAddEntry); err != nil {
			return OpenResult{}, err // ErrDeletePending
		}
	}
	h, f, created, dir, err := l.resolveOrCreate(ctx, c, r) // disposition: open, create, overwrite, supersede (§5.10 for a create)
	if err != nil {
		return OpenResult{}, err
	}
	want := r.Access
	if err := l.authorize(ctx, c, h, r.Parent, want); err != nil { // §4.6: via the parent, so DELETE comes from delete-child
		return OpenResult{}, err
	}
	o, g, err := l.open.Open(ctx, c.Client, f.ID, toOpenState(r)) // RFC 14 §6: deny modes; a handle-caching grant over a conflict
	switch {                                                         //   is broken first and the open answered ErrDelay(DelayRecall)
	case errors.Is(err, ErrDelay):
		return OpenResult{}, err // no worker waits (§3.2)
	case err != nil:
		return OpenResult{}, err
	}
	max, _ := l.granted(ctx, c, h, r.Parent) // MaximalAccess, without a backup intent's privilege
	return OpenResult{Ref: refOf(o, h), File: f, MaximalAccess: max, Grant: g, Created: created, Dir: dir}, nil
}
```

**A sharing violation is decided after handle caching is broken.** A request
whose deny-mode check fails against opens that a handle-caching grant covers —
an SMB lease with handle caching keeps handles open that its client has already
closed — **MUST NOT** be refused yet, whatever its protocol: an SMB or NFSv4
open, an anonymous NFSv3 read or write, I/O under a delegation, an NFS `REMOVE`
or `RENAME` against a deny-delete
([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)).
It starts a break of those grants and returns `ErrDelay` (§3.2), and on its
retry re-checks the deny modes against the opens that remain. Refused at once,
it fails against handles nobody is using.

**Delete on close is a mark and a deferred unlink.** An open sets or clears the
mark with `SetAttr`, `AttrChange.DeleteOnClose`, through the open, or at create
(`OpenRequest.DeleteOnClose`); the shard records it with open state's
`SetDisposition` ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)).
When the file's last open closes, `Close` itself runs the deferred unlink of the
recorded entry, as the principal that marked it and through the marking open's
`OpenGrant`, so that open's grant — a backup or restore intent's privilege
included — authorises it ([§4.6](#4.6%20One%20chokepoint),
[RFC 7 §8.2](rfc-7-namespace-metadata.md#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later)),
and returns the parent's `WCC`. One still refused is logged at `Warn`, counted
(§7), and the file keeps its name: the client has closed, and no reply reaches
it.

**The shard runs every release.** It calls the engine's `Release`
([RFC 8 §8.1](rfc-8-engine.md#8.1%20A%20removal%20is%20one%20transaction%2C%20then%20batches))
at the file's primary, at four moments: when `Unlink` or `Rename` reports a file
left with no entry and no open holds it; when open state's `Close` reports the
last close of a file with no entry, having deleted its durable open record
([RFC 14 §9.1](rfc-14-open-state.md#9.1%20An%20open%20keeps%20a%20file%20alive));
when a release commits that wrote the pending releases of its file's named
streams ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)),
for each of those streams; and in recovery, for every pending release, once the
shard's grace has ended, read with `PendingReleases(share, shard)`, never by a
scan of the share's files. The release transaction re-checks its holders itself
([RFC 7 §4.5](rfc-7-namespace-metadata.md#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction)),
so a release begun on a stale view aborts and loses nothing.

### 5.4 Remove and rename

The metadata store applies the namespace change. The final unlink writes the
file's pending-release record in the same transaction, and holders are only the
durable open records; the release transaction deletes the record once no open
remains ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)),
and the shard runs it (§5.3). A crash between the unlink and the release leaks
nothing. A rename that replaces a target applies the same rule to the target.

```go
// unlink serves Unlink and Rmdir (rmdir set).
func (l *Local) unlink(ctx context.Context, c Caller, dir Target, name []byte, rmdir bool) (UnlinkResult, error) {
	f, err := l.meta.Namespace.Lookup(ctx, c.Resolved(), dir.H, name) // ErrNotFound
	if err != nil {
		return UnlinkResult{}, err
	}
	unlock := l.order.OpenState(dir.H, f) // §5.0: both files' steps, in file-ID order
	defer unlock()
	if rmdir && f.Type != Dir {
		return UnlinkResult{}, ErrNotDir // Rmdir
	}
	// Delete-child on the parent, or delete on the file; the sticky rule; immutable parent.
	// Through an open, its grant decides (§4.6, RFC 7 §7.7).
	if err := l.authorizeName(ctx, c, dir, f, Delete); err != nil {
		return UnlinkResult{}, err
	}
	// Opens of others: a deny-delete refuses after handle caching is broken (§5.3);
	// a grant on the file is recalled (ErrDelay); an SMB delete of a directory with
	// an open child is ErrShareViolation (RFC 14 §9.5).
	if err := l.open.CheckChange(ctx, f.ID, c.Client, ChangeRemove); err != nil {
		return UnlinkResult{}, err
	}
	var r UnlinkResult
	var orphan Orphaned
	l.order.Accept2(dir.H, f, func(v Versions) { // draws the directory's and the file's versions (RFC 7 §9.4)
		r.Dir.Before = l.captureBefore(ctx, dir.H) // nil for a directory over NFSv3 (§5.7)
		// One transaction: the entry, Nlink − 1, usage, and for the last link the
		// pending release (RFC 7 §4.1, §4.3); ErrNotEmpty for a directory with entries;
		// a cross-shard unlink runs as the front's coordinator's prepare (RFC 15 §4.2).
		orphan, err = l.meta.Namespace.Unlink(ctx, c.Resolved(), v, dir.H, name, grantOf(dir.Open))
		r.Dir.After = l.overlaid(ctx, dir.H)
	})
	if err == nil && orphan.File != 0 {
		err = l.engine.Release(ctx, orphan.File) // aborts while an open holds it (RFC 7 §4.5); the last close releases (§5.3)
	}
	return r, err // the chunks it referenced are dropped by the engine's batches and GC (RFC 8 §8.1)
}
```

`Rename` takes the same steps for the source entry and, when the target name
exists, for the target:

```go
func (l *Local) Rename(ctx context.Context, c Caller, from Target, fromName []byte, to Handle, toName []byte, o RenameOpts) (RenameResult, error) {
	src, _ := l.lookup(ctx, c, from.H, fromName)
	dst, exists := l.lookup(ctx, c, to, toName)
	unlock := l.order.OpenState(from.H, to, src, dst) // §5.0
	defer unlock()
	switch {
	case exists && o.NoReplace:
		return RenameResult{}, ErrExist
	case exists && dst.Type == Dir && src.Type != Dir, exists && dst.Type != Dir && src.Type == Dir:
		return RenameResult{}, typeMismatch(dst) // RFC 7 §5.4: ErrIsDir or ErrNotDir
	case exists && dst.Type == Dir && !empty(dst):
		return RenameResult{}, ErrNotEmpty
	}
	if err := l.open.CheckChange(ctx, fileOf(to), c.Client, ChangeAddEntry); err != nil { // ErrDeletePending: the target directory is marked (§5.4)
		return RenameResult{}, err
	}
	if err := l.authorizeRename(ctx, c, from, src, to, dst); err != nil { // delete-child at from, add-entry at to; through an open its grant
		return RenameResult{}, err
	}
	// A rename of a delete-pending file, a deny-delete on src or a replaced dst, and an
	// SMB rename of a directory with an open child are refused; grants are recalled
	// (RFC 14 §7, §9.4, §9.5). An SMB rename over a target open by anyone is refused (RFC 22 §13).
	if err := l.open.CheckChange(ctx, src.ID, c.Client, ChangeRename); err != nil {
		return RenameResult{}, err
	}
	if exists {
		if err := l.open.CheckChange(ctx, dst.ID, c.Client, ChangeRemove); err != nil {
			return RenameResult{}, err
		}
	}
	var r RenameResult
	var orphan Orphaned
	var err error
	l.order.Accept2(from.H, to, func(v Versions) { // WCC of both directories in the same hold
		r.From.Before, r.To.Before = l.captureBefore(ctx, from.H), l.captureBefore(ctx, to)
		// One transaction; the loop check — a directory into its own subtree, ErrInvalid —
		// is inside it (RFC 7 §5.2); ErrCross(CrossProject) across tree quotas (RFC 7 §2.8).
		// Across shards the front's coordinator prepares at each primary (RFC 15 §4.2).
		orphan, err = l.meta.Namespace.Rename(ctx, c.Resolved(), v, from.H, fromName, to, toName, grantOf(from.Open))
		r.From.After, r.To.After = l.overlaid(ctx, from.H), l.overlaid(ctx, to)
	})
	if err == nil && orphan.File != 0 {
		err = l.engine.Release(ctx, orphan.File) // the replaced target; aborts while an open holds it (RFC 7 §4.5)
	}
	return r, err
}
```

**A delete-pending directory takes no new entries.** Before it applies a create,
a link or a rename, the shard **MUST** check the open state of the destination
parent and refuse the call if that directory is marked delete on close
([RFC 7 §8.2](rfc-7-namespace-metadata.md#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later)),
whatever the protocol, as it refuses a new open of the directory. The metadata
store cannot see the mark, so no other layer checks it.

### 5.5 Size changes

A `SetAttr` that changes size, and a `Deallocate`, are authorised and checked
like a write, clear setuid and setgid as a write does (§5.1 step 5), then are
applied by the engine's `Truncate` or `Deallocate` at the file's primary; the
other attributes of the same `SetAttr` are applied by the metadata store in the
same service call. A `SetAttr` that sets `mtime` or `atime` to a given time is
applied through an existence commit after every write staged before it
([RFC 7 §9.5](rfc-7-namespace-metadata.md#9.5%20An%20explicit%20time%20outlives%20the%20writes%20staged%20before%20it)).

### 5.6 Quota

The shard is the one enforcer of quotas. Neither the metadata store nor the
engine refuses a write for quota.

```go
// Reserve, at step 4 of a write, a create (one inode) or a copy's destination.
func (q *quota) Reserve(ctx context.Context, c Caller, f File, bytes int64) (*Reservation, error) {
	for _, s := range []Scope{QuotaShare, QuotaProject, QuotaUser, QuotaGroup} { // §4.3's order
		lim, ok := q.limit(ctx, s, c, f) // a principal's own quota, else the share's default for its kind
		if !ok {
			continue
		}
		used := q.committed(ctx, s, f) + q.unfolded(ctx, s, f) // per-shard totals plus unfolded deltas (RFC 16 §4.4)
		if used+q.reserved(s, f)+bytes > lim.Hard(clock) {      // soft limit as hard once its grace has run
			q.events.Emit(ctx, QuotaEvent{ /* hard refusal */ })
			return nil, &QuotaError{Scope: s}
		}
		if q.reserved(s, f)+bytes > q.slack(lim) { // this primary's uncommitted share: min(S, f × Hard)
			return nil, &Delay{Cause: DelayCommit} // waits for a commit, or retries
		}
	}
	return q.add(c, f, bytes), nil // released by the existence commit that records the charge, or on failure
}
```

- A reservation is volatile, held in the shard's memory: after a crash the
  journal re-applies the writes and their charge ([RFC 8](rfc-8-engine.md)).
- **Overshoot bound.** A primary sees its own reservations, not other
  primaries', so writers through `k` primaries exceed a hard limit by at most
  `(k − 1)` times the slack, plus what transactions committing concurrently add.
  `S` and `f` are settings of [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings);
  the derivation is [RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)'s
  counters, and this section owns only the slack rule above.
- **Soft limits.** Crossing the advisory threshold or the soft limit emits a
  quota event (§3.3) and refuses nothing; the metadata store records when the
  soft limit was first exceeded, and once the grace time has run from then the
  soft limit is enforced as the hard one. Falling back under it clears the
  record. The time is the store's `Now`, so the grace time ends up to the
  clock-offset bound σ early or late; a quota's grace is not a fence and
  tolerates it.

### 5.7 GetAttr

The metadata store is a leaf: it never calls the engine. `GetAttr` joins
`Files.Get` with the engine's `Overlay` — `Size`, `Times` and `Version` of writes
staged but not yet committed — at the file's primary, where both are local. The
overlay wins where it is newer. The two are read under the file's commit
serialisation, retried as a sequence lock, so a commit between them never yields
a lower size or `Version` than one already returned
([RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives)).

**The overlay's `Version` advances on every write the primary accepts**, at
acceptance, not at the existence commit that later records it. Each change draws
its `Version` from the journal's version counter, so it rises across restarts
and primaries too ([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)).
A `GetAttr` after a write is acknowledged returns a `Version` greater than any
returned before it. The existence commit **MUST** leave the committed `Version`
no lower than any the overlay reported for the writes it records, so the change
attribute never moves backward when the overlay drains.

This is the only read path for a file's attributes; `ReadDir` with attributes
does the same join per entry, batched per primary, and so do `Lookup` and every
result that carries a `File`. A `WCC`'s `Before` is returned only when the
primary captured it and accepted the operation in one hold of the file's accept
lock — not under the commit serialisation, which lets another write be accepted
between the capture and the operation — and never for a directory over NFSv3;
where the lock cannot be taken it is nil. `After` is read in the same hold once
the change is staged. An adapter answers a reply's attributes from the `WCC` and
never from a `GetAttr` after the call, which could include another client's
change landed between the two.

### 5.8 The write verifier

The NFS write verifier is the engine's, returned by its `Write` and `Commit`
and derived as [RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)
says; the shard passes it through unchanged. `Write` returns the verifier
sampled before the bytes are staged, and `Commit` the one sampled once the sync
it waits for has completed, so a loss before or during that sync makes the
`Commit`'s verifier differ from its writes', and the client resends them.

**SMB learns through its flush.** SMB has no verifier. A `Commit` through an SMB
open (SMB `FLUSH`) takes the file's loss sequence from the engine's `Commit`,
read once the sync completes ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)),
and passes it to open state's `Flushed`, which fails the flush with `ErrLost`
once for each open that was open across a loss of an acknowledged, unoffloaded
write of the file ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).

### 5.9 Copy and clone

`Copy` (NFSv4.2 `COPY`, SMB copychunk) and `Clone` (NFSv4.2 `CLONE`, SMB
duplicate-extents) share `CopyArgs` and run as the engine's clone
([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)):
a durable clone spec, the source's journal-held runs copied and synced, then its
carved refs adopted, all resolved at one point in the source's history.

- **`Clone` is atomic.** The whole extent is one clone, answered only when it
  is done. A request longer than `clone_max_len`
  ([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings))
  is refused with `ErrInvalid`, all or nothing, and **(cluster)** one between
  files of two shards, whatever their primaries, with `ErrCross`
  (`CrossShard`), changing nothing, so the client copies the bytes itself.
- **`Copy` is not atomic.** The engine's `Copy` runs bounded chunks, each a
  small atomic clone, and returns the bytes copied; at the caller's deadline it
  stops after its current chunk and answers a short count, from which the
  client continues. A chunk refused after others are done answers the same
  short count; only a refusal of the first chunk returns its error. Between
  shards the front copies the bytes itself, reading at the source's primary and
  writing at the destination's in bounded chunks, so no adapter composes a copy
  (§4.1). A copy is always synchronous, as copychunk and NFSv4.2 `COPY` are
  served today; RFC 14's asynchronous copy state
  ([RFC 14 §2.7](rfc-14-open-state.md#2.7%20Copy)) is unused until a client is
  shown to need offload (§9).

1. admit and route both handles, and authorise read on the source and write on
   the destination, as §5.1 steps 1–3 do for each;
2. refuse with `ErrInvalid` a copy or clone whose source and destination are one
   file and whose extents overlap, as Linux and RFC 7862 refuse it; with
   `ErrCross` (`CrossNamespace`) one RFC 8 §9.1's admission rule refuses, since
   no chunk can be adopted across namespaces; with `ErrIncomplete` one whose
   source is `Incomplete`, before anything changes;
3. reserve the destination's charge (§5.6), and clear setuid and setgid on the
   destination as a write does (§5.1 step 5);
4. call the engine.

**Calls behind a running clone wait, within what the adapter allows.** While a
clone or a copy's current chunk runs, a write, truncate, deallocate or clone
into its source extent, a write into its destination extent, and a truncate,
deallocate or clone of its destination file or the destination's release, is
held at the shard until that clone or chunk ends and then runs (§5.0), so each
clone reads one consistent source and no removal of the destination races its
batches. How long it may wait is the call's own:

```go
// WithHold bounds the service's one wait inside a call: behind a running clone
// or copy chunk. Zero, or no WithHold at all, answers ErrDelay(DelayClone) at once.
func WithHold(ctx context.Context, max time.Duration) context.Context
```

A call that waits past its `max`, or its deadline, is answered `ErrDelay` with
`DelayClone` — unless the clone it waits on is itself held up by a stalled
metadata store, a remote outage or a restart's wait for space
([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing),
step 4), when it is answered with that cause (`DelayCommit`, `ErrUnavailable` or
`DelayCapacity`). An open of a destination a restart has not yet served answers
`ErrDelay`, with `DelayCapacity` while its clone waits for space and
`DelayClone` otherwise. Answering at once by default would cost an NFSv3 client
its fixed retry-later pause, several seconds, for a chunk that ends far sooner;
so each adapter passes `WithHold`, and which calls it lets wait, and how many at
once per session or connection, is its count, not the service's
([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)). The service counts
nothing about holds.

The clone holds the source open, so a last close or unlink does not release it
mid-clone. A source run the engine reads as Lost — a source ref whose chunk
record is gone included — is carried to the destination as Lost, never as zeros.
A clone that is refused leaves the destination's prior content; once begun it is
not undone but resumed after a crash from its durable spec, and is answered only
when done.

### 5.10 Lookup and create

**Lookup.** The front admits the call and asks the catalog; the shard joins the
overlay. A directory walk is the client's: the service resolves one name per
call and never follows a symlink — the client does
([RFC 22 §12.4](rfc-22-smb.md#12.4%20Reparse%20points)).

```go
func (f *Front) Lookup(ctx context.Context, id Identity, dir Handle, name []byte) (Handle, File, error) {
	if f.pseudo.Is(dir) {
		return f.pseudo.Lookup(ctx, id, dir, name) // a pseudo directory, or the junction into a share
	}
	c, err := f.admit(ctx, id, dir) // §4.9
	if err != nil {
		return nil, File{}, err
	}
	switch string(name) {
	case ".":
		file, err := f.getattr(ctx, c, dir)
		return dir, file, err
	case "..":
		if f.isShareRoot(dir) { // ".." at a share root answers the root itself; NFSv4 LOOKUPP leaves to the pseudo-fs
			return f.parentOfRoot(ctx, c, dir)
		}
	}
	// Traverse (execute) on dir is checked inside RFC 7's Lookup, through the chokepoint (RFC 7 §7.1).
	h, file, err := f.catalog.Lookup(ctx, c, dir, name) // ErrNotFound; a name compared by the share's case rule (RFC 7 §3.3)
	if err != nil {
		return nil, File{}, err
	}
	if share(h) != share(dir) { // a junction into another share: admitted and resolved again there
		if _, err := f.admit(ctx, id, h); err != nil {
			return nil, File{}, err
		}
	}
	file, err = f.overlay(ctx, c, h, file) // the file's shard: §5.7
	return h, file, err
}
```

**Create.** At the parent's primary:

```go
func (l *Local) Create(ctx context.Context, c Caller, dir Handle, name []byte, a CreateArgs) (CreateResult, error) {
	if err := validName(name, l.share(dir)); err != nil { // RFC 7 §3.2, §3.8: bytes, length, names every protocol can use
		return CreateResult{}, err
	}
	unlock := l.order.OpenState(dir)
	defer unlock()
	if err := l.open.CheckChange(ctx, fileOf(dir), c.Client, ChangeAddEntry); err != nil {
		return CreateResult{}, err // ErrDeletePending (§5.4)
	}
	// add-file or add-subdirectory on the parent (RFC 7 §7.7)
	if err := l.authorize(ctx, c, dir, Handle{}, addRight(a.Kind)); err != nil {
		return CreateResult{}, err
	}
	res, err := l.quota.Reserve(ctx, c, l.fileFor(dir), inode) // one inode, in §4.3's scope order
	if err != nil {
		return CreateResult{}, err
	}
	defer res.ReleaseUnlessCommitted()
	shard := l.placement(dir, a.Kind) // RFC 11 §2.2: a per-child shard may take a new directory
	var r CreateResult
	l.order.Accept(dir, func() {
		v := Versions{dir: l.engine.Draw(ctx, dir)}
		v[newFile] = l.drawAt(ctx, shard) // at the new file's primary when it lands in another shard (§4.8)
		r.Dir.Before = l.captureBefore(ctx, dir)
		// One transaction: owner and group from the resolved caller after squashing; the
		// inherited ACL; a case-fold collision is ErrExist under the share's case rule
		// (RFC 7 §3.3); an exclusive create stores the verifier, and a retry whose
		// verifier matches returns the file, any other ErrExist (RFC 7 §2.10).
		r.Handle, r.File, err = l.meta.Namespace.Create(ctx, c.Resolved(), v, dir, name, attrsOf(c, a))
		r.Dir.After = l.overlaid(ctx, dir)
	})
	return r, err
}
```

RFC 7's `Attrs` carries no exclusive-create verifier field, though
[RFC 7 §2.10](rfc-7-namespace-metadata.md#2.10%20Exclusive%20create) stores one;
that is a gap of §10.

### 5.11 Locks

```go
func (l *Local) Lock(ctx context.Context, c Caller, ref OpenRef, r LockReq) error {
	if r.Wait && len(r.Ranges) != 1 {
		return ErrInvalid // only a single range may wait (RFC 22 §7)
	}
	if err := l.authorizeLockState(ctx, c, ref); err != nil { // NotYours for another principal (§3.1)
		return err
	}
	taken := make([]ByteRange, 0, len(r.Ranges))
	for _, rg := range r.Ranges {
		// RFC 14: grace (ErrGrace unless r.Reclaim), conflicts with every protocol's
		// locks, a recall of a conflicting grant (ErrDelay, or ErrBlocked with a waiter
		// when waiting), the owner's sequence (StateError BadSeqID).
		err := l.open.Lock(ctx, c.Client, ref.File(), r.Owner, ref.Open, rg, r.Exclusive, r.Wait, r.Reclaim, r.Seq)
		if err != nil {
			l.unlock(ctx, c, ref, r.Owner, taken) // all or nothing: release what this call took
			return err                            // ErrBlocked: a waiter is registered; LockWaitOver answers it
		}
		taken = append(taken, rg)
	}
	return nil
}
```

A waiting lock holds no worker and has no deadline; it ends only as RFC 14's
waiter does ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)).

## 6. Invariants, and the test of each

Every test runs in the service suite (§8) unless it names another layer.

| # | Invariant | Test |
| --- | --- | --- |
| V1 | Adapters hold only `vfs.Service`; no adapter holds a metadata view, an engine, open state or a component. | [RFC 20 §7.1](rfc-20-adapters.md#7.1%20Imports%20and%20call%20sites)'s import test. |
| V2 | Every operation that needs authorisation reaches `Files.Authorize` through `authorize.go` alone. | The import and call-site test of §8 fails a call of `Authorize` or `Granted` outside it. |
| V3 | Every cross-component order is the one in §5, in every protocol. | The shard suite drives each operation and asserts the order of calls on recording metadata and engine fakes. |
| V4 | A routing refusal never reaches an adapter. | **(cluster)** Move a shard's primary between a call's routing and its arrival; assert the call succeeds. |
| V5 | The service persists nothing and imports no adapter; the front imports no store, open state or engine. | The import test of §8. |
| V6 | A recall not acknowledged by its deadline revokes the grant; no worker waits on a recall. | A conflicting operation returns `ErrDelay` while its recall is pending, with no goroutine blocked; a recall that misses its deadline delivers `RevokedGrant` naming the grant, `Renew` reports `RecallableRevoked`, and a write under its stateid returns `ErrRevoked`, not `NotYours`. |
| V7 | The conflict check and the I/O it admits run at one primary, and no conflicting state is granted between them. | A model test interleaves a lock and a write: the lock is ordered wholly before or after the write. |
| V8 | Every open-state call names its `ClientID`, and an ID the client does not hold is refused. | An `OpenID`, `GrantID` or `WatchID` presented by another client returns `NotYours`. |
| V9 | The share grant is evaluated on every call. | A client removed from a share grant is refused on its next call with a handle it already holds; an SMB `MAXIMUM_ALLOWED` open whose parent grants delete-child includes `DELETE`, and after the parent's group changes so it no longer does, a cached delete-child decision is not reused. |
| V10 | Quota is enforced only by the shard, and overshoot stays within §5.6's bound. | Writers through `k` primaries overshoot by no more than the bound, counted per run, on a 1 GiB project quota as on a 1 TiB one; a write refused by a share-wide, project, user and group quota in turn returns a `*QuotaError` naming each scope, and with two exceeded the first in §4.3's order; `StatFS` inside a project tighter than its share reports the project's, and the share's once the project's quota is deleted. |
| V11 | (cluster, [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)) A forwarded mutation retried after a lost reply returns its original result and applies once. | Loopback layer: a forwarded write retried after a dropped reply, with another write between, applies once. |
| V12 | Every call that names a handle or an open is admitted — state, flavour, client rules, resolution — whatever path the handle arrived by. | For each of lock, unlock, test-lock and watch, reached by a handle presented directly and by a walk from the pseudo-root: a quiesced share returns `ErrDelay`, a share refusing the flavour `ErrWrongSecurity`, a client outside its rules `ErrAccess`; a compound crossing from a share squashing root into one that does not acts as root only in the second. |
| V13 | No mount, tree connect or session is stored; a reconnecting client is admitted afresh. | A renamed share keeps serving held handles and established tree connects, and a new `Root` needs the new name. |
| V14 | Every node builds the same pseudo-filesystem from the same share list. | Pure unit: two fronts built from one share list return byte-identical pseudo handles, numeric ids and change attributes, and a share added on one is seen on the other with a raised change attribute. |
| V15 | No adapter resolves a share, admits a call, or builds any part of the pseudo-filesystem. | `Root`, `SecInfo`, `ListShares` and pseudo handles are driven in the front suite with no adapter; every case of V12 runs once per entry (`Root` with a path, with a name, and the zero `ShareRef` then `Lookup`). |
| V16 | The `Version` `GetAttr` returns advances on every accepted write and never moves backward. | Client A reads `Version`, B writes and is acknowledged, A's next `GetAttr` is greater before any existence commit and not lower after it; 16 readers `GetAttr` while writes commit in a loop and none sees a `Version` lower than one it saw; a `chmod` drawn below a write committed or staged at a higher version is redrawn above it. |
| V17 | Every release is run by the shard at the file's primary, at the four moments of §5.3. | An unlinked file whose last open closes is released, and so, after a restart, is a file whose pending release was written before the crash; a shard that never calls `Release` leaks both. |
| V18 | An open's grant authorises a call only for the principal it was granted to. | bob presents alice's open to write a file alice may write and bob may not, then one both may write: `NotYours` both times and alice's open intact. A service that trusts the open's grant for any presenter accepts the first; one that re-authorises bob afresh accepts the second. |
| V19 | A write through a persistent open, or any open on a continuously available share, is stable; a stable write waits for the sync and, overwriting committed content, its existence commit. | A write on a persistent open, asked unstable, survives a crash injected after its reply; a stable append's reply waits for no metadata transaction, counted by the store's transaction counter; a stable overwrite stalls with the store and answers `ErrDelay` at the deadline, never success. |
| V20 | A journal-limit or capacity refusal is `ErrDelay` while space can be freed, never `ErrNoSpace` for a deadline; `ErrNoSpace` and `ErrQuota` are answered at once. | With the journal full of dirty bytes and the remote reachable, a write returns `ErrDelay(DelayCapacity)` and succeeds on retry once offload frees space; with the store unreachable past the deadline, still `ErrDelay`; with the store refusing puts as denied, `ErrNoSpace` at once. |
| V21 | A deny-mode refusal is decided only after the handle-caching grants covering the conflicting opens are broken. | An SMB open, an NFSv4 `OPEN` and an NFSv3 `WRITE` meeting a deny mode held only in an SMB handle cache each get `ErrDelay`, the lease is broken, and the retry succeeds. |
| V22 | An unprivileged caller's write, truncate, deallocate, copy or clone into a setuid or setgid file — the owner's included — clears the bits in a committed change before the bytes are staged. | bob writes alice's setuid file and `GetAttr` before any commit shows the bit clear; again with alice, unprivileged, then with a deallocate, a copy and a clone; an unprivileged append to a setuid, append-only file succeeds and clears the bit, and a write below its size is `ErrPerm` and clears nothing. |
| V23 | A blocked lock holds no worker and has no deadline; nothing answers one other than as granted before `CancelLock` returns. | NLM and SMB locks that wait are answered `ErrBlocked` and later granted through `LockWaitOver`, with no goroutine held between; an NFSv4 one is `ErrLocked` and its next poll after the unlock is granted; a lock list with one range refused leaves none of the call's ranges held. |
| V24 | I/O under a delegation is served only to the client holding it and authorised for the caller's own principal. | An NFSv4.1 client with a write delegation writes as a second principal that may write the file, then a third that may not: the first accepted, the second `ErrAccess`; another client presenting the stateid gets `NotYours`. |
| V25 | `Write` samples the verifier before staging and `Commit` after its sync; an SMB flush across a loss fails once with `ErrLost`. | A `Commit` that waits on a failed sync window returns a verifier other than its writes'; the verifier changes after an unstable write is dropped as lost in the same process and epoch, and when a shard is handed away and back, and not across a shard-epoch raise that moves nothing; an SMB flush through each of two opens of such a file fails once, then succeeds. |
| V26 | Copy and clone refuse overlap, a clone over `clone_max_len`, a namespace or (cluster) shard crossing and an `Incomplete` source, each changing nothing; a copy across shards is the service's; a Lost source run is copied as Lost; a clone is atomic, a copy may be short. | Each refusal returns its error and leaves the destination unchanged and readable; a `COPY` whose deadline passes partway answers a short count and the destination holds exactly that many bytes; a clone of a source with a Lost run reads Lost there at the destination. |
| V27 | A request held after `ErrGrace` or `DelayFrozen` outlives the grace or freeze, bounded by §3.2's table. | `Delay.Hold()` is a pure unit over every cause; through the SMB adapter ([RFC 20 §7.4](rfc-20-adapters.md#7.4%20Cross-protocol%2C%20through%20both%20adapters)), an open held through a full grace and a write held through a two-minute freeze complete when they end. |
| V28 | A create, link or rename into a directory marked delete on close is refused, over every protocol. | With a directory marked over SMB, a create, a link and a rename into it, over SMB and over NFS, are each refused. |
| V29 | Every call takes open-state steps, then fold admission, then accept locks, each in file-ID order; a held call holds none of them and re-runs §5.1's steps 3–6. | A write and an unlink of the file's last name race 10⁴ times with a deny-delete open: neither waits for its deadline and no deny-delete is granted between the unlink's check and its commit; a 1 GiB clone of a VM disk under writes, with a lease break pending: each held write gives up its open-state step and accept lock, the break's acknowledgement and a new open proceed during the clone, and each held write completes after re-running its checks. |
| V30 | The service waits inside a call only behind a running clone or copy chunk, and at most the call's `WithHold`. | An NFSv3 write into a cloning extent with `WithHold(10s)` is held and completes when the clone ends; with `WithHold(0)` it returns `DelayClone` at once. |
| V31 | Every mutating call returns the `WCC` of each object it changed, from one hold of its accept lock. | For each mutating method, a concurrent write between the call and a later `GetAttr` does not appear in the returned `WCC`; a directory's `Before` is nil over NFSv3. |
| V32 | Within the shard package, the engine and the metadata write views are called only from the orchestration files. | The call-site test of §8. |
| §3.2 | A callback path moves; notifications honour traverse; a break that times out leaves its opens usable. | A recursive watch reports nothing below a directory its holder cannot traverse; an SMB lease whose break times out leaves its open usable for the holder's next write. |
| §3.2 | Revoked state is reported and freed by cause. | An administrator revokes a client holding an open, a lock and a delegation: each stateid answers `ErrRevoked` with `Admin` set, `Renew` reports `AdminRevoked` only, its record and sessions remain, and after `FreeRevoked` of each it opens afresh under the same client ID. |
| §3.2 | A removed or moved share ends its state with `RevokedShare`. | With a waiting lock and an outstanding watch on a share, remove it, then repeat with a move naming an address: the recording `Callbacks` receive `RevokedShare` naming it, with `Addr` set for the move; the share's opens and deny modes are gone; the removal's step 3 waits for a create admitted before the mark. |
| §4.3 | Every `ErrDelay` carries its cause; grace and lost state answer as RFC 14 says. | Each `ErrDelay` the service returns carries its `DelayCause`, a freeze's its bound; restart with an NFSv4.0 client holding a lock: its stateid answers `StaleState` during grace and is reclaimed, and after grace a freed one answers `BadState`. |
| §4.6 | A privileged handle is privileged for every request through it. | A backup-intent handle sets owner, group and DACL on a file whose DACL excludes the caller, as `robocopy /B /SECFIX` does; without backup intent each is refused; a rename through a handle opened with `DELETE` succeeds without delete-child. |
| §5.3 | A delete on close runs at the last close, through the marking open. | A backup-intent open with delete on close of a file whose DACL excludes its caller, and an EA set through it, succeed; with the parent made immutable before the close, the deferred unlink is refused, counted in `vfs_deferred_unlink_refusals_total`, and the file keeps its name. |
| §5.9 | An `Incomplete` source refuses before anything changes. | Copy and clone from a file a restored snapshot lists as incomplete into an existing 2 GiB file: `ErrIncomplete` each, the destination unchanged. |
| §3.1 | SMB multichannel loses state only with the session's last channel. | Losing one channel leaves the session's opens; losing its last disconnects only that session's opens. |

## 7. Observability

Metric names are shown without the deployment's prefix.

| Answers | Metric | Type |
| --- | --- | --- |
| How fast is each operation, per protocol? | `vfs_op_seconds{op, protocol, outcome}` | histogram |
| How often are clients told to retry, and why? | `vfs_delayed_total{op, cause}` | counter |
| How often does quota refuse, and how much is reserved? | `vfs_quota_refusals_total{kind}`, `vfs_quota_reserved_bytes` | counter, gauge |
| Are callbacks failing? | `vfs_callback_errors_total{kind}` | counter |
| Are events being lost? | `vfs_events_dropped_total{stream}` | counter |
| How long do calls wait behind a running clone (§5.9)? | `vfs_clone_wait_seconds{outcome}` | histogram |
| How often is a delete on close refused after its client closed (§5.3)? | `vfs_deferred_unlink_refusals_total` | counter |

Operation latency and quota metrics are owned here and nowhere else. Held
requests are the adapters' ([RFC 20 §10.1](rfc-20-adapters.md#10.1%20Metrics));
forwarding and epoch refusals are RFC 15's; recalls are RFC 14's.

Each service call opens a trace span that parents the shard's, the metadata
store's and the engine's spans. No share label on per-operation metrics
([RFC 16 §8.1](rfc-16-metadata-store.md#8.1%20Metrics)).

## 8. Conformance

The service is tested in layers, cheapest first, as
[RFC 20 §7](rfc-20-adapters.md#7.%20Tests) tests an adapter. Each layer catches
something the ones above it cannot.

| Layer | Proves | How | Runs |
| --- | --- | --- | --- |
| pure units | admission over (share, identity); pseudo-filesystem determinism (V14); `Delay.Hold()` for every cause; quota slack arithmetic; the lock-order sorter; the ID codecs round-trip and refuse malformed bytes; `vfs.Errors()` lists every declared error | table tests | per change |
| shard suite | §5's orders and the V-rows of §6 | `shard.Local` over the in-memory store, a real engine and real open state | per change |
| front suite | routing, admission, coordination across shards | the front against a recording `Shard` and `Catalog` | per change |
| fault injection | stalls, conflicts, a full journal, Lost content, a client that never acknowledges, a callback that fails, time | fakes at the metadata views, the engine, the `Recaller` and `Callbacks`, and the clock | per change |
| model | lock, write, open and unlink interleaved against a sequential model, under `-race` | property tests | short per change; 10⁴ interleavings nightly |
| loopback | the same suite with a `Router` that returns a remote `Shard` over a loopback node channel: the route envelope, dedup (V11), cursor safety | `suite.Run` with the remote factory | after merge, gated on the first remote view shipping |
| adapter side | translation only | `vfstest.Record` and `vfstest.Null` ([RFC 20 §7.2](rfc-20-adapters.md#7.2%20Handlers%20against%20a%20recording%20service)) | per change |

Splitting front from shard is what makes a failure point somewhere: a front-suite
failure is routing or admission, a shard-suite failure is ordering. Running the
same suite over loopback is what makes "the caller cannot tell a remote shard
from a local one" a checked claim rather than a design intention.

**Entry point.** `vfstest/suite.Run(t, newService)` runs the conformance suite
against any factory, one file per area — names, attributes, data, locks,
clients, shares, copy, faults — as `pkg/metadata/storetest` does for stores.
The recording fake, the null service and the recording `Callbacks` in
`pkg/vfs/vfstest` are generated by `go:generate` from `service.go` with the
`go/types` generator in `internal/tools/genvfstest`; CI runs
`go generate ./... && git diff --exit-code`, so a new method is recorded without
hand edits and a stale fake fails the build.

**Import and call sites.** Beside [RFC 20 §7.1](rfc-20-adapters.md#7.1%20Imports%20and%20call%20sites)'s
test, which covers adapters, a test walks `internal/vfs` and fails on an import
of the metadata store, open state or the engine (V5), and walks
`internal/vfs/shard` and fails on a call of the engine or a metadata write view
outside §2.4's orchestration files, or of `Authorize` or `Granted` outside
`authorize.go` (V2, V32). It is seeded with one deliberate violation in its own
test data and must report it.

**Benchmarks.** Driven by the same factory, so a protocol benchmark minus a
service benchmark is the adapter's own cost:

- per operation, ns and allocations: `GetAttr`, `Lookup`, `Read` and `Write` of
  4 KiB and 1 MiB, `Open`, a `ReadDir` page; **0 allocations** on the data path;
- service overhead against the direct store or engine call, collocated and over
  loopback;
- contention: N writers on one file, N openers on one file, quota reservation
  under `k` primaries;
- recall latency: a conflicting write to its acknowledgement, and to a forced
  break at 35 s.

| Tier ([RFC 29 §4.3](rfc-29-test-harness.md#4.3%20Tiers), [§4.10](rfc-29-test-harness.md#4.10%20Benchmarks%20and%20long%20runs)) | What |
| --- | --- |
| per change | each benchmark for one iteration, as a smoke test |
| after merge | `benchstat` against the stored baseline |
| nightly, dedicated host | tracked and not blocking at first; three nights below the 7-day median adds a note to the develop-status issue |
| manual | contention sweeps and long model runs |

## 9. Migration

The service is introduced in slices, each one PR or a few, each leaving the
tree working:

| Slice | What moves | What is deleted |
| --- | --- | --- |
| 0 | `vfs.Service`, with the shard half delegating to today's `metadata.Service`; `vfstest`'s recording fake and null service | nothing |
| 1 | shares, identity and admission: `Root`, `SecInfo`, `ListShares` and the pseudo-filesystem into the service; a cross-protocol test that a quiesced or netgroup-restricted share is refused on every entry path | `v4/pseudofs`, `shareEntryStatus`, the NFSv3 permission cache. An adapter's own admission check stays until every operation on its path calls the service: removed earlier, it fails open |
| 2 | the write path — the four copies of the write sequence become §5.1, returning `WCC` | the adapters' copies |
| 3 on | open and close, locks, the namespace, the rest | each adapter's copy as its last caller moves |

- **The RFC moves with the code.** A slice that needs a signature change edits
  this RFC in the same PR, and a reviewer rejects code that disagrees with it.
- **Copy stays synchronous**, with a short count at the deadline, as copychunk
  and NFSv4.2 `COPY` are today; RFC 14's `CopyBegin` and `CopyStatus` are unused
  until a client is shown to need offload.
- **The chokepoint shrinks visibly.** A test lists every adapter import of the
  metadata store, the block store and the lock manager, and every direct
  `Authorize` call, against a checked-in allowlist. It fails on a new entry and
  on a stale one, so the list only shrinks and each PR shows its deletions; it
  is seeded with one deliberate violation in its test data. At empty it is
  replaced by [RFC 20 §7.1](rfc-20-adapters.md#7.1%20Imports%20and%20call%20sites)'s
  import test and §8's, which also checks that only the shard imports the store
  and the engine.

> decision: slices, each with its RFC edit, and a shrinking allowlist.
> Rejected: one up-front amendment of every RFC before code, which goes stale
> before the first slice lands; leaving the signatures to the implementing
> issue, where no reviewer reads them; and code review alone, or the final
> import test alone, neither of which shows progress or stops a new leak in
> the meantime. Overturned if two slices are found to need landing together.

## 10. Open questions

1. **Compound batching.** Adapters run compounds as sequences of calls. A batch
   call (a `LOOKUP`-`GETATTR`-`OPEN` chain in one service call) pays only across
   the network, since collocated calls are function calls.

   > decision: no batch call now. Rejected: a batch call before it is
   > measured; an adapter cache of service answers, which
   > [RFC 20 §2.2](rfc-20-adapters.md#2.2%20What%20an%20adapter%20may%20hold)
   > forbids. Overturned by §8's service-overhead benchmark over the loopback
   > remote shard showing compound round trips limiting a split deployment.
2. **pNFS layouts.** Not a parity requirement, and not in the service
   (§3.1). When a pNFS RFC lands, a separate `Layouts` interface joins the
   service, its calls over RFC 14's layout state; a layout is bound to the
   (shard, epoch) it was granted under ([RFC 14](rfc-14-open-state.md)).
3. **ID widths.** The shard-number and `OpenID` widths of the stateid and the
   SMB `FileId` must be one choice (§3.4's ponytail).
4. **Gaps in the layers below**, found by §3.5's table: RFC 14 declares no call
   for `TestState`, `ReleaseLockOwner`, `Downgrade` or `ConfirmOpen`, though
   RFC 21 §4.2 and §5.2 rely on each; RFC 7's `Attrs` has no exclusive-create
   verifier though §2.10 stores one, and no call turns an empty file into a
   symlink (SMB `SET_REPARSE_POINT`); RFC 16 declares no read view of the share
   name and path indexes or of quota records but `ControlPlane`'s, which the
   catalog must not hold. Each is the owning RFC's to add.

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
