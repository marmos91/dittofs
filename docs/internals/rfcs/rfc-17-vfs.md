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
one reception desk checks every request against the same rules.

The service is one interface with two implementations. The **filesystem**,
`*vfs.Filesystem`, admits the call to its share, checks opens and locks, reserves
quota and drives the storage, with nothing in between. A file's calls run in
the filesystem at the file's **primary**, the node in charge of the file. In a
cluster each node also runs a **gateway**, which sends each call to the file's
primary (to its own filesystem when this node is the primary) and decides nothing.
On a single node there is no gateway, and the adapters call the filesystem. An
adapter never learns which node holds a file.

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
2. The call reaches the filesystem at the file's primary, through the gateway in
   a cluster. The filesystem admits it to the `profiles` share, checks the open's
   permission and the locks others hold, reserves quota if the file grows, and
   hands the bytes to the engine, which stages them in the journal. alice-pc
   gets OK.
3. build01's write arrives through the NFS adapter as a write on an
   **anonymous open**, which carries no state. The same path checks it against
   the deny modes others hold, finds alice's, and refuses it as a sharing
   violation. The NFS adapter turns that into an NFS error.

One rule, in one place, for both protocols.

**Reaching a share works the same way.** Resolving a share, admitting the call
and the NFSv4 **pseudo-filesystem** belong to the service, and an adapter keeps
no share list of its own. Admission runs on every call, not once at mount
([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)).

![The six steps of every call — route (the gateway's, in a cluster), then in the filesystem admit, authorise, check open state, reserve quota, engine — run in one order for both protocols: alice-pc's 64 KiB SMB write through her own open passes all six and gets OK, while build01's NFSv3 write on an anonymous open is refused at step 4 by alice's deny mode as ErrShareViolation, which the NFS adapter turns into an NFS error](img/rfc17-six-steps.svg)

**The words you need.**

- **adapter** — the code for one wire protocol: framing, compounds, replay
  caches, error codes, and nothing else ([RFC 20](rfc-20-adapters.md)).
- **filesystem** / **gateway** — `*vfs.Filesystem`, which decides
  every call at the file's primary / `*vfs.Gateway`, which routes each call
  there and decides nothing; the **gateway node** is the node a client's call
  arrives at ([§2](#2.%20Gateway%20and%20filesystem)). "Shard" keeps
  [RFC 11](rfc-11-ownership.md)'s meaning, a group of files with one primary.
- **primary** — the node that owns the file's shard and holds its
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
  permissions; the service's `authz` package is its only caller ([§4.6](#4.6%20One%20chokepoint)).
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
- No worker waits on a client ([§3.2](#3.2%20Callbacks)).
- Adapters never see the cluster: a call sent to the wrong node is re-routed
  inside the service, and a call the gateway forwarded and retried after a lost
  reply is applied once
  ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)).
- Quota is enforced here and nowhere else, and writers spread over several
  primaries overshoot a hard limit by a stated bound at most.

### Scenarios

How a call crosses from its gateway node to the file's primary, and how a retried
write is answered once, are [RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)'s
and [RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)'s scenarios;
this RFC adds nothing to them. An NFS write against an SMB lease is
[§3.2](#3.2%20Callbacks)'s recall diagram. One more is this RFC's:

**An administrator removes bob's grant on `profiles` while bob-pc still holds handles.**

1. bob-pc's next call presents a handle it got earlier.
2. The filesystem admits the call, and the metadata store's one permission
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

![The layers: NFS and SMB adapters speak wire only and call one vfs.Service, which a gateway routes to the file's primary, whose filesystem alone calls the metadata store (RFC 16), the content engine (RFC 8), open state (RFC 14) and identity (RFC 18), calls adapters back only through the Callbacks interface they implement, and emits quota and access events to a sink it never waits on](img/rfc17-layers.svg)

[RFC 0 §1.3](rfc-0-data-lifecycle.md#1.3%20The%20layers) draws the same picture with the roles marked.

Beyond the one place for every check that the example in "In plain words"
shows, one service buys two things:

1. **The topology is invisible to adapters.** A call whose primary is another node
   is forwarded by the gateway; an adapter never learns there was a hop.
2. **It is testable without a protocol.** The service has its own suite (§8),
   and the protocol suites then test translation only.

**The name.** Package `vfs`, interface `vfs.Service` — the field's word for the
protocol-neutral file API, since `Filesystem` is taken.

### 1.1 Non-goals

The service **MUST NOT**:

- encode or decode a wire format, run a compound or keep a per-session replay
  cache — those are the adapters' ([RFC 20 §2.2](rfc-20-adapters.md#2.2%20What%20an%20adapter%20may%20hold));
- evaluate a file permission itself — it asks the one chokepoint ([§4.6](#4.6%20One%20chokepoint));
- hold a copy of metadata, open state or content that outlives the call that
  read it, except the caches keyed by the version of everything they read —
  the admission cache ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)) and the
  authorisation cache ([§4.6](#4.6%20One%20chokepoint)) — and the
  pseudo-filesystem, a function of the share list's version;
- persist anything of its own. Every durable fact is recorded by the component
  that owns it;
- import an adapter. Calls upward go through interfaces adapters implement
  ([§3.2](#3.2%20Callbacks));
- count or budget the requests an adapter holds pending: that is the adapter's
  ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)) and its memory
  RFC 24's;
- answer DFS referrals or NFSv4 referrals to another installation. A share is
  served only by the installation that holds it.

## 2. Gateway and filesystem

`vfs.Service` (§3.1) is the one interface the adapters call. Two types in
`internal/vfs` implement it, and a third function receives what one of them
sends:

| Piece | What it is | Does | Never |
| --- | --- | --- | --- |
| `*vfs.Filesystem` | the filesystem; a file's calls run in it at the primary of the file's shard ([RFC 11](rfc-11-ownership.md)) | everything a call decides: admission to the share, identity resolution, flavour and squash ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)); the order of every step (§5); open state (§2.2), metadata and content | routes a call |
| `*vfs.Gateway` | the `Service` the adapters hold in a cluster | sends each call to its file's primary (§2.1): to the checked `Service` `Serve` returned when that is this node, otherwise with the route envelope ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)) through `Transport` to `Serve` there. Within the caller's deadline it re-routes a routing refusal and resends a lost reply's call under the same request ID; it traces and counts each send (§7). One generic call path serves every method | admits, resolves an identity, squashes, calls the metadata store or the engine, holds open state, or answers a call itself, except `ErrUnavailable` while it stops or when no primary answers within the caller's deadline |
| `vfs.Serve` | the checks every call to a primary passes, forwarded or local | fences each call (the sender's node epoch, the shard epoch, and a hop count of zero), answers a retry from the dedup table by request ID, and otherwise calls the filesystem. It receives from `Transport`, and returns the same checks as a **checked `Service`** for the gateway's local path | forwards: it holds the `*Filesystem`, so it cannot reach the gateway |
| `vfs.Router`, `vfs.Transport`, `vfs.Peers` | interfaces declared in `vfs` and implemented outside it, so `vfs` imports no network code | the router places a handle from RFC 11's shard records ([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20primaries)) and fences an arriving envelope; the transport carries a call over a node channel ([RFC 15 §4.4](rfc-15-topology.md#4.4%20Node%20channels)); `Peers`, implemented by the transport, carries the filesystem's own calls to another primary (§2.3) | — |

![The separation: on a single node the adapters call vfs.Service, which is the *vfs.Filesystem, and the filesystem alone calls the metadata store, the engine, open state and identity. On node A, a gateway node in a cluster, the adapters call the same vfs.Service, which is a *vfs.Gateway that routes each call through the Router; a call whose primary is node A goes to node A's own filesystem through the checked vfs.Service that Serve returns, which fences and dedups it as it does a forwarded call, and any other call goes with its route envelope over the Transport to vfs.Serve on node B, the file's primary, which fences on node epoch, shard epoch and hop 0, dedups by request ID and calls node B's filesystem, which decides the call. Node B is itself the gateway node for its own clients. The gateway holds a vfs.Service, never a *vfs.Filesystem](img/rfc17-gateway.svg)

**Adapters sign in, the gateway forwards, the filesystem decides.** An adapter
authenticates its client through the service's own `SignIn`, which the filesystem
delegates to [RFC 18](rfc-18-identity.md)'s authenticator, and passes a
`Call` carrying the `Identity` it proved in every call. `vfs.Service` is an adapter's only
dependency on the core: it holds no identity interface of its own. The gateway carries that
`Identity` without reading it. The filesystem resolves it on the share the call
reaches, then admits, authorises and runs the call. An adapter is handed a
`vfs.Service` once and cannot tell which implementation it holds.

| Deployment | Builds | Adapters are handed |
| --- | --- | --- |
| a single node | `*vfs.Filesystem` | the filesystem |
| each node of a cluster | the filesystem, `Serve` over it, and the gateway over the checked `Service` `Serve` returns | the gateway, whose local `Service` is that checked `Service`, never the bare filesystem |

Every node runs the whole filesystem, so the identity resolver runs on every
node. A cluster differs from a single node only by `Serve` and the gateway.
Why every node runs both roles, and what would justify splitting them, is
recorded in [RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20node%20type)'s ponytail.

> decision: the cut between nodes is `vfs.Service`, not the interface between
> the service and the engine. With the cut at `Service`:
>
> - open state lives in one place, the file's primary, beside the I/O it gates
>   (§4.8);
> - a client `WRITE` is one forwarded call carrying its bytes;
> - the chatty engine interface stays local: `Draw`, `Overlay`, `Write` and
>   `Commit` are called under one accept lock (§5.1), and no network can hold
>   that lock;
> - the one shard epoch that `Serve` checks fences the whole call, where a lower
>   cut would have to fence each store and engine call on its own.
>
> Rejected: running the service at the gateway node over a remote engine and
> remote open state. That splits open state across two nodes and puts a hop
> between every conflict check and its I/O. Overturned if a call is found that
> needs a decision the primary cannot make.

> decision: the gateway holds a `Service`, never a `*Filesystem`. It therefore
> cannot reach past the interface into the filesystem's state, and its unit tests
> run it over `vfstest.Record`. §8's architecture check enforces it.

### 2.1 What runs where

The gateway picks a call's primary from the arguments the call names. The rule
is data, one row per kind of argument:

| The call names | The gateway routes it to |
| --- | --- |
| a handle, or an open (by `OpenRef.Handle`) | the primary of that handle's shard |
| two handles: `Rename`, `Link` | the coordinator that [RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries) names: the source directory's primary, or the target directory's for a link |
| two handles: `Copy`, `Clone` | the destination's primary |
| an ID that names its shard ([RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references)) and no handle: `AckRecall`'s `GrantID`, `Unwatch`'s `WatchID`, `TestState`'s and `FreeState`'s `StateRef` | the primary of that shard; for `TestState`, of its first `StateRef`'s, whose filesystem asks each other one's primary through `Peers` |
| no file: the rest of the `Clients` area, the `Principals` area, `Root`, `SecInfo`, `ListShares`; or a pseudo-filesystem handle | the local filesystem, which is the lease owner of the clients connected to its node; a call that names no file runs on the node that received it and is never forwarded |
| a handle the router cannot place | the local filesystem, which admits the call before it answers `ErrStale` (V39), so the gateway refuses nothing |

The gateway keeps nothing past a call, and `Serve` keeps only the dedup table.
The filesystem keeps the admission cache, the pseudo-filesystem, quota
reservations, the admitted-call counts, and the client and open state of §2.2.

> decision: one component calls the metadata store and the engine: the filesystem.
> That way a transaction boundary, a retry, a fence and a cache key are each
> decided in one place. With two callers, each would pick a different retry for
> the same conflict and key its cache on different fields. Overturned only if a
> read path is measured needing a store read that going through the filesystem
> makes slower.

> ponytail: a read that any node could serve
> ([RFC 15 §4](rfc-15-topology.md#4.%20Where%20each%20call%20runs)'s "any node"
> row) is still routed to the primary. Serve it at the gateway node's own filesystem,
> under [RFC 11 §6](rfc-11-ownership.md#6.%20Reads%20on%20other%20nodes), when
> §8's benchmark through the gateway shows the hop limiting a read-heavy
> workload.

> decision: the service is a new layer, not a grown metadata store. The
> metadata store shrinks to the store operations beneath it, and RFC 16's store
> stays swappable under the service. The orchestration of §5 lives in the
> filesystem. Rejected: growing today's `pkg/metadata.Service` into the service,
> which keeps orchestration and storage in one package that a second backend
> would have to reimplement; and rewriting both in one go, which no reviewer
> can follow (§9). Overturned if a second store backend is dropped from the
> plan, so that nothing below the service needs to stay swappable.

### 2.2 Who owns open state and locks

| Who | Holds | Never |
| --- | --- | --- |
| **adapter** ([RFC 20](rfc-20-adapters.md)) | the wire form of a waiting lock: the request to answer, the NLM host to call back | decides a conflict |
| **gateway** | nothing | holds any of the open-state table: it routes `Lock`, `Open`, `Close` and the rest to the file's primary |
| **filesystem, as lease owner** (the node holding the client's sessions) | each NFSv4 client's unconfirmed proposal ([RFC 14 §2.9](rfc-14-open-state.md#2.9%20Clients%20pending%20confirmation)), each client's lease record and callback channel ([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)); relays recalls and grants to the connection | — |
| **filesystem, at the file's primary** | [RFC 14](rfc-14-open-state.md)'s open-state table: opens, deny modes, byte-range locks of NLM, NFSv4 and SMB alike, delete pending, caching grants, watches, lock waiters, and in a cluster layouts. It is held as the concrete `*openstate.Table` in the filesystem's `Deps`, of which the filesystem keeps only the views (§2.3), and the filesystem implements its `Sink`, `Closed` included, which the node hands to the table's `Start` rather than its `Deps`, since the table is built first. The filesystem runs the conflict check and the I/O it admits under the file's accept lock with nothing between ([§4.8](#4.8%20One%20primary%20per%20file), V7) | — |
| **metadata store** ([RFC 16](rfc-16-metadata-store.md)) | only what must survive a restart: client records, persistent SMB opens, the durable open records that keep an unlinked file alive | an ordinary lock |

The table is in memory, keyed by file, never by name or handle. It is lost when
a primary fails over and rebuilt by grace reclaims for that shard alone
([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)).
A lock costs one hop to the primary and no store write. One shard's locks are
decided by one node, so locking scales across shards, not within one hot shard.

> decision: open state is its own component, neither in the metadata store nor
> in the engine. In the store, every lock would be a store transaction, a
> remote one in a cluster, a network hop from the I/O it gates; the store, a
> protocol-neutral leaf, would grow timers, grace and callbacks upward, and
> every backend would have to pass them. In the engine, unlink, rename and
> delete on close, which need open state and never call the engine, would have
> to go through it, and the engine would learn principals and deny modes. The
> filesystem holding the table's step while it calls the engine already gives the
> check and the I/O one step (§4.8). Overturned if one of the two components is
> found to need open state for a decision of its own.

> decision: there is no separate distributed lock manager. The conflict check
> must sit with the I/O it gates, at the primary, or something can be granted
> between them ([§4.8](#4.8%20One%20primary%20per%20file)); a lock manager on
> other nodes would add a hop and a window to every write. Overturned if one
> shard's lock traffic is measured saturating its primary while the files it
> serves cannot be split across shards ([RFC 11](rfc-11-ownership.md)).

### 2.3 Dependencies and wiring

The node builds each piece once, from its own `Deps` struct, in `node.New`
([RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop)). A field is
an interface where it has two implementations and a concrete type where it has
one, as in [RFC 20 §3.4](rfc-20-adapters.md#3.4%20Interface):

```go
package vfs // internal/vfs

type Deps struct {
	Meta     *kvstore.Store    // RFC 16 §3.2 (pkg/metadata/kvstore): the views the filesystem declares
	Open     *openstate.Table  // RFC 14: open state and client records (§2.2); New keeps only its views
	Engine   Content           // RFC 8's engine, or a fake in unit tests (§8)
	Identity identity.Resolver // RFC 18 §6: real, or a fake in unit tests; internal to the filesystem
	Auth     identity.Authenticator // RFC 18 §3.1: behind vfs.Service's SignIn (the Principals area)
	Names    identity.Identities    // RFC 18 §4.1: behind vfs.Service's Spell and Find (the Principals area)
	Peers    Peers             // (cluster) the transport; nil on a single node, where every primary is this one
	Events   Events            // the node's event sink, or the null sink (§3.3)
	Clock    Clock // the package's own subset of RFC 8 §2.1's Clock
	Metrics  *metrics.Registry
	Tracer   trace.Tracer // RFC 25 §8.1: spans; the no-op tracer when tracing is off
	Logger   *slog.Logger
}

// Content is the engine methods §3.5 lists, declared where the filesystem calls them.
type Content interface{ /* Draw, Overlay, Write, Commit, Read, Delete, … */ }

// New builds the filesystem and starts nothing (§2.5). *Filesystem implements vfs.Service.
func New(d Deps) (*Filesystem, error)

type GatewayDeps struct {
	Local     vfs.Service // the checked Service Serve returned, never the bare filesystem (§2)
	Router    Router
	Transport Transport
	Metrics   *metrics.Registry
	Logger    *slog.Logger
}

// NewGateway builds the gateway. *Gateway implements vfs.Service.
func NewGateway(d GatewayDeps) (*Gateway, error)

// Serve receives calls from t: r.Fence, then the dedup table, then s. It
// returns the same checks as checked, which the gateway calls for a primary that
// is this node, so a local call is fenced and deduplicated as a forwarded one is.
// stop ends both (§2.5).
func Serve(fs *Filesystem, t Transport, r Router, m *metrics.Registry) (checked vfs.Service, stop func(context.Context) error, err error)

// Router places a call, and fences one that arrives, from RFC 11's shard
// records (RFC 15 §6).
type Router interface {
	Route(ctx context.Context, h vfs.Handle) (env Envelope, local bool, err error)
	Fence(ctx context.Context, env Envelope) error // a shard epoch not current here, a fenced node epoch, a hop above zero
}

// Transport carries a call to env.To and its reply back, over a node channel (RFC 15 §4.4).
type Transport interface {
	Send(ctx context.Context, r Request) (Reply, error)
	Receive(h func(context.Context, Request) Reply) (stop func())
}

// Peers carries the filesystem's own calls to another primary (cluster), each
// under the route envelope and received by Serve there, fenced and deduplicated
// as a forwarded call. The transport implements it; the gateway never does,
// since a filesystem calling through the gateway would route through itself.
type Peers interface {
	Prepare(ctx context.Context, to NodeID, p Prepare) (Prepared, error) // RFC 15 §4.2: prepare a two-primary call
	Outcome(ctx context.Context, to NodeID, op []byte, commit bool) error // RFC 15 §4.2: its commit or abort
	Overlay(ctx context.Context, to NodeID, hs []Handle) ([]File, error)  // §3.5: a lookup's or listing's entries in another shard, batched per primary
	ReadAt(ctx context.Context, to NodeID, src OpenRef, off int64, dst []byte) (int, error) // §5.9: a cross-shard copy's source read
	Sequence(ctx context.Context, to NodeID, o LockOwner, seq uint32) error // §5.11: a sequenced owner's number at its home primary (RFC 14 §2.8)
	ClientState(ctx context.Context, to NodeID, c ClientID, op Op, args []any) (Reply, error) // the lease owner to each primary holding its client's state, TestState's other refs included (§3.5)
	Recall(ctx context.Context, to NodeID, s SessionRef, ev any) error      // a primary's recall, notify or revoke to the node holding s, the session the service picked (§3.2); zero s: to an NFS or NLM client's lease owner, the client named in ev (RFC 14 §4.1)
}

// Envelope is RFC 15 §4.3's route envelope; its rules are there.
type Envelope struct {
	Request    []byte // the request ID the adapter set with vfs.WithRequestID (§4.7)
	To         NodeID // the primary
	Shard      ShardID
	ShardEpoch uint64
	Node       NodeID // the sender
	NodeEpoch  uint64
	Hop        uint8
}

// Request is one Service method as values (§4.7), sent to the node that owns
// the share, and Reply is its results. Forwarding is what the gateway does with
// a Request, not a type: vfs.Call names only who is calling.
type Request struct { Env Envelope; Op vfs.Op; Args []any; Trace []byte }
type Reply   struct { Results []any; Err error }
```

`New` keeps no `*openstate.Table`: it takes the table's seven views
([RFC 14 §11.3](rfc-14-open-state.md#11.3%20Methods)) as fields, and each
area of §3.1 calls only the ones it needs — `Locking` calls `openstate.Locks`,
`Grants` and `Watches`; `Data` calls `Opens` and `Checks`; `Names` calls
`Checks`; `Attributes` calls `Checks` and `Opens`; `Clients` calls
`openstate.Clients`, `Recovery` and, for `ReleaseLockOwner`, `Locks`. Starting
and stopping the table is the node's (§2.5).

The router and the transport are RFC 15's, built over the shard records and
the node channel; the transport also implements `Peers`, whose methods are every case where a
filesystem needs a second primary; §3.5's table names each use. `Serve` takes the
`*Filesystem` because it also hands `Peers` calls to it, which are no `Service`
methods. The open-state table is node-local and calls no other node: when the
table at a client's lease owner ends the client, it calls the filesystem's
`Sink.ClientEnded`, and the filesystem ends the client at each primary its record
names through `Peers.ClientState`
([RFC 14 §4.3](rfc-14-open-state.md#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)).

> decision: `Serve` takes the `*Filesystem`, not a `Service`, because a `Peers`
> call (a prepare, an owner sequence, a recall) has no `Service` method to
> land on. It still calls only the filesystem, so it can never reach the gateway.
> Overturned if `Peers` calls get a narrow receiving interface of their own;
> `Serve` then takes that and a `Service`. The adapters are not this RFC's to build: `node.New` builds
each one last, as [RFC 20 §3.4](rfc-20-adapters.md#3.4%20Interface) and
[§4.7](rfc-20-adapters.md#4.7%20Wiring%3A%20what%20a%20protocol%20plugs%20into%20%60core%60)
say, and hands it the filesystem or the gateway as `core.Deps.Service` (§2), and
nothing else of the service.

### 2.4 Package layout

```text
pkg/vfs/                     the contract adapters build against
  service.go                 Service, composed of one interface per area (§3.1)
  types.go open.go           Handle, Identity, Call, OpenRef, Target, WCC, … and the aliases of metadata types (§3.0)
  ids.go                     the codecs of every client-held name (§3.4)
  errors.go                  Errors() (§4.3), and every value of vfserr re-exported as an alias
  vfserr/                    the filesystem's error values: sentinels, *Delay, *QuotaError, *StateError,
                             *CrossError, all of DelayCause and CrossKind, and Extent; standard library only
  callbacks.go               Callbacks, Revocation (§3.2)
  events.go                  Events, AccessEvent and QuotaEvent (§3.3)
  vfstest/                   Record, Null and RecordingCallbacks, hand-written (§8)
    suite/                   suite.Run: the conformance suite, one file per area (§8)
internal/vfs/                the filesystem, one file per operation; the gateway and its receive side
  filesystem.go              Filesystem, New(Deps), Start, Drain, Stop, JoinShare (§2.5); Content
  gateway.go                 Gateway, NewGateway: §2.1's routing table and the one generic call path
  serve.go                   Serve: the fence and the dedup table, and the checked Service it returns
  envelope.go                Envelope, Request, Reply, Router, Transport, Peers
  clients.go                 Connect, AddSession, Renew, Disconnect, Destroy, … (§3.1)
  lookup.go readdir.go       Lookup (§5.10); ReadDir
  create.go link.go          Create (§5.10); Link
  unlink.go rename.go        Unlink and Rmdir; Rename (§5.4)
  getattr.go setattr.go      GetAttr (§5.7); SetAttr, size changes included (§5.5)
  acl.go xattr.go            GetACL, SetACL; Xattrs, SetXattr, RemoveXattr, Streams
  access.go                  Access
  open.go close.go           Open, Downgrade, ConfirmOpen (§5.3); Close and delete on close
  read.go write.go           Read, Seek (§5.2); Write (§5.1)
  commit.go                  Commit, and the write verifier (§5.8)
  deallocate.go copy.go      Deallocate; Copy and Clone (§5.9)
  lock.go grant.go watch.go  Lock, CancelLock, TestLock, Unlock (§5.11); AckRecall; Watch, Unwatch
  shares.go                  Root, SecInfo, ListShares, StatFS, Quota, Snapshots (§4.9)
  release.go grace.go        the four moments of release (§5.3); grace and pending-release recovery
  arch_test.go               imports, call sites and the gateway's fields (§8)
```

- **A subpackage is a boundary its imports enforce, or a pure leaf.** `authz`
  is a package so "only it calls `Authorize`" is an import rule; `admit`,
  `pseudofs`, `lockorder` and `quota` are packages so their tests import
  nothing but a narrow fake. Each declares the few reads it needs beside its own
  code.
- **One file per operation.** `rename.go` holds `Rename`, including the
  two-primary path for directories at two primaries (RFC 15 §4.2), so a reader
  finds an operation in one file. The operations call each other: a rename needs
  the lock order, the release and quota. So they stay one package, with no
  import cycles to manage.
- **The gateway's three files call no store, engine, open state or identity**,
  and hold the filesystem only as a `vfs.Service`. §8's architecture check enforces
  both.

### 2.5 Lifecycle

The node builds, starts and stops every piece: `node.New`, `Node.Start` and
`Node.Stop` ([RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop)).
In tests, the tier's own constructor does it (§8). An adapter never does.
`Node.Start` starts the pieces in build order and `Node.Stop` stops them in
reverse. No piece closes anything it was handed.

| # | Piece | Built by | Calls | Start | Stop |
| --- | --- | --- | --- | --- | --- |
| 1 | metadata store, engine, GC, open state, identity | their RFCs, in [RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop)'s order | engine: the journal, the syncer, the block-metadata views; GC: `Blocks`, `Remote`, the syncer, `Keys` | before the filesystem | after it; GC drains before the filesystem stops |
| 2 | `*vfs.Filesystem` | `vfs.New(Deps)` | identity, open state, the metadata views, the engine, the event sink | `Start`, below | `Drain`, then `Stop` |
| 3 | `vfs.Serve` **(cluster)** | `vfs.Serve` | `Router.Fence`, the filesystem | registers on the transport | after the node has handed over the shards it is primary of; stops receiving and joins the calls running |
| 4 | `*vfs.Gateway` **(cluster)** | `vfs.NewGateway(GatewayDeps)` | the checked `Service`, `Router`, `Transport` | nothing to start | refuses new calls with `ErrUnavailable` and joins the forwarded ones |
| 5 | adapters | [RFC 20 §3.4](rfc-20-adapters.md#3.4%20Interface) | `vfs.Service` | last: after the first reconcile has opened the shares and, in a cluster, a node epoch exists | first |

So the start order is: the metadata store, the engine, GC, open state and
identity, the filesystem, `Serve`, the gateway, the first reconcile, the adapters.
No adapter listens before then, so no call meets a filesystem with no shares.
The stop order is: the adapters, the gateway, **(cluster)** the handover of
every shard this node is primary of
([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)),
`Serve`, the GC drain, the filesystem, open state and identity, the engine, the
metadata store. The handover runs while `Serve` still receives, so other nodes'
calls to those shards are answered by this node until the new primary serves,
never refused while they wait for a failover.

`vfs.Service` carries no lifecycle method. `*vfs.Filesystem`, which `New` returns
and only the node holds, carries three, and `JoinShare` for a share's removal:

```go
func (s *Filesystem) Start(ctx context.Context) error // before any adapter serves
func (s *Filesystem) Drain(ctx context.Context) error // after every adapter has drained
func (s *Filesystem) Stop() error                     // closes nothing it was handed
func (s *Filesystem) JoinShare(ctx context.Context, sh ShareID) error // waits out the calls admitted to sh before its removal mark (§4.9)
```

`Stop`, not `Close`: `Data.Close` closes an open, and `Stop` matches
`Node.Stop` and `Table.Stop`.

The filesystem moves through four states, **built**, **serving**, **draining** and
**stopped**, once and in that order. `New` starts no goroutine and opens nothing.

**Start.** Each step completes before the next begins. A step that fails stops
what the earlier ones started, and `Start` returns its error, so the filesystem is
either serving or as it was built.

1. **Open state.** For each shard this node is primary of, call RFC 14's
   `Recovery.ServeShard`, which installs the shard's persistent opens before
   anything is answered, and enter grace
   ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)):
   a call that needs, or conflicts with, state that may still be reclaimed
   answers `ErrGrace` until the shard's grace ends. Arm the lease, recall and
   lock-waiter timers, and run the recovery of pending releases when the table
   raises `Sink.GraceEnded` for the shard ([§5.3](#5.3%20Open%20and%20close)). `Start` does not wait for
   grace to end.
2. **Shares.** Read the share list, build the pseudo-filesystem from it
   ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)), start following the share
   list's version.

**Drain.** Called once every adapter has drained
([RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states)), and in a
cluster once `Serve` has stopped, because an
adapter's drain still delivers break acknowledgements, `CLOSE`s and unlocks
into the service. The filesystem then refuses every new call with `ErrUnavailable`
and joins the calls already admitted. At the context's deadline it cancels
them, and a mutation cancelled there may still have applied, as RFC 20 §3.1
says of an abandoned request. Draining revokes nothing: open state is left to
the next start's grace, and a recall in flight is neither forced nor revoked.

**Stop.** Stops the timers. It closes nothing it was handed: the node stops
open state (`Table.Stop`), then closes the engine and the store, after it.

**The filesystem keeps no event queue.** It hands each event to the sink as it
builds it, and the sink's contract is that it never blocks (§3.3): the sink
drops what it cannot keep and alone counts each drop, RFC 25's
`events_dropped_total{stream, reason}` (§7). With no sink configured the null
sink takes them. The node stops the sink after the filesystem
([RFC 25 §8.1](rfc-25-observability.md#8.1%20What%20the%20root%20builds)), so
a second queue here would only drop the same event in a second place, counted
by a second metric.

**Shares come and go while the filesystem serves.** Adding a share is a record
written to the store; the filesystem sees the share list's version rise and
rebuilds the pseudo-filesystem, and no call to the service is made. Removing
one is [RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)'s
procedure, whose service steps are the node's: for every shard it is primary
of whose share's removal has reached step 3 and whose `rm` row is not yet
offloaded — while serving, and again at `Node.Start` — it calls `JoinShare`,
§4.9's join, then RFC 14's `DropShare`. The control plane's `DeleteShare` only marks
the share and returns, `202` ([RFC 23 §3.2](rfc-23-control-plane.md#3.2%20A%20share)), so no
request waits on the slowest call, and a crash between the mark and the join
leaves a marked share the next start finishes.

> decision: the lifecycle methods are on the concrete types, not on
> `vfs.Service`. Keeping them off the interface is what stops an adapter from
> draining the service under the other protocol. Overturned if a caller that
> did not build the service needs to drive it.

> decision: no option starts the filesystem with grace already over. A test
> advances its fake clock past the grace period; a setting that skips grace
> would let a filesystem serve conflicting state before its clients reclaim it.

## 3. Interface

Signatures are indicative; the obligations are normative.

### 3.0 Value types

Every type an adapter passes or receives is in `pkg/vfs`. Types the metadata
store and open state define — `ClientID`, `OpenID`, `GrantID`, `WatchID`,
`SessionRef`, `StateRef`, `OpenRef`, `RefKind`, `Seqs`, `ClientStatus`, `GraceRef`, `Revocation`, `RevokeReason`, `LockOwner`, `ByteRange`, `LockRange`, `File`, `Attrs`, `ACL`,
`Xattr`, `Principal`, `FilesystemInfo`, `Usage`, `Quota`, `CachingGrant`,
`LockWaiter`, `ChangeMask`, `ShareID` — are re-declared there as aliases
(`type File = metadata.File`), so an adapter imports one package and the
import rule of [RFC 20 §2.3](rfc-20-adapters.md#2.3%20What%20an%20adapter%20may%20import)
holds.

| Type | What it is |
| --- | --- |
| `Identity` | what the adapter proved, not yet mapped through any share's policy, in neutral terms: `Proof`, how it was proved — `None`, `Claimed` (asserted, not proved), `Secret` (a shared-secret challenge, such as NTLM), `Ticket` (a third party's mutual proof, such as Kerberos); `Protection`, how its messages are protected — `None`, `Integrity` (every message signed), `Privacy` (every message sealed); `Claims`, the uid, gid and groups a client asserted, marked as claims and never trusted as proof; the client's address; and `Evidence`, what sign-in established — `Name`, the proven `Principal` or zero for none, `SIDs`, `DenyOnly`, `Expires` ([RFC 18 §3.1](rfc-18-identity.md#3.1%20The%20authentication%20interface)) — which key every cache, so a cache stays keyed on what was proved ([RFC 18 §7.1](rfc-18-identity.md#7.1%20Every%20key%20carries%20the%20whole%20proof)); never the session key. No protocol name, credential flavour or mechanism level: each adapter maps its own to `Proof` and `Protection` ([RFC 21 §11.4](rfc-21-nfs.md#11.4%20Open%20state), [RFC 22 §16.4](rfc-22-smb.md#16.4%20Open%20state)). Every method takes one inside a `Call`; the filesystem resolves it per share ([RFC 18 §6.1](rfc-18-identity.md#6.1%20From%20Identity%20to%20caller)), except a call that names no share, such as the `Clients` area's, which is mapped under the installation's defaults, never a share's ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)) |
| `Call` | `{Identity Identity; Protocol Protocol; Client ClientID; Session SessionRef; Channel struct{Seq uint16; Replay bool}}`: who a call comes from, taken by every method of every area but `SignIn` and `Spell`, which act for no caller (§3.1). `Identity` is the user, what the resolver resolves and caches; the rest sit beside it, never inside it, so no cache ever sees them and two machines signed in as one principal share a resolution but never a client. `Protocol` is the request's; `Client` the machine, as the adapter proved it — for NFSv4.1 and SMB its session's, for NFSv4.0 the client on the state its stateid resolves to, or for an operation that names an open or lock owner, that owner's client, and only a client confirmed under NFSv4.0, as [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open) defines it, for NLM the host's client record, zero for an NFSv3 call that names no lock; `Session` the SMB or NFSv4.1 session, zero otherwise; `Channel` an SMB request's channel sequence and whether it is a replay, checked through `Opens.Channel` ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)). It is not `Identity.Client`, which is the client's address. One shape serves every call: an NFSv4 `REMOVE` or an SMB delete on close can name an open (`Target.Open`) whose `NotYours` check needs the call's client, and every access event carries the protocol and client (§3.3); a call that names no open, a `Lookup` say, leaves `Client` unused. A `Clients` method names the client it acts on as its own argument; only `Propose` and `Connect`, which come before there is one, take a `Call` whose `Client` is not yet that client |
| `Handle` | opaque bytes naming a file or a pseudo directory (§3.4, [§4.4](#4.4%20Handles%20are%20opaque)) |
| `OpenRef` | RFC 14's, aliased: `{Kind RefKind; State StateRef; Open OpenID; Handle Handle}` ([RFC 14 §11.3](rfc-14-open-state.md#11.3%20Methods)). `Kind` is `Opened`, `Locked` (a lock state), `Delegated` or `Anonymous`; an NFS reference is its stateid, `State`, whose `Seq` is the seqid presented (zero: current; an older one is `StateError` `OldState`); an SMB one is its `OpenID`, its shard in `State.Shard`. It is the only place that seqid travels: `Seqs` carries the owner sequence numbers only. It names no client — the call's client is the one the adapter proved (`Call.Client`) — and carries no identity: the call's `Identity` is compared with the principal the open was granted to, a lock state's open's for a `Locked` one (§3.1). `vfs.Anonymous(h)` builds an anonymous one, `vfs.Delegated(g, h)` one for I/O under a delegation |
| `Target` | `{H Handle; Open *OpenRef}`: what a mutation acts on — the file for an attribute change, the directory for a name change — and, when set, the open the request came through, whose grant authorises it ([§4.6](#4.6%20One%20chokepoint)). `Open` is always an open of the file the change acts on — for a name change, the named entry's file, as an SMB rename or delete through its `FileId` sends — never an open of the directory in `H`; it is the `via` RFC 14's `CheckChange` takes, which refuses an open of any other file ([RFC 14 §2.4](rfc-14-open-state.md#2.4%20CachingGrant)) |
| `Cursor` | where a listing resumes: the 63-bit digest of the last entry returned ([RFC 7 §3.5](rfc-7-namespace-metadata.md#3.5%20A%20cookie%20survives%20concurrent%20mutation)), zero to start |
| `DirEntry`, `Page`, `EntryFields` | one listed entry — name, handle, its digest as cursor, and its `File` when asked; a page `{Entries []DirEntry; Next Cursor; EOF bool}`; which fields a listing fills |
| `OpenRequest`, `OpenResult` | disposition, access, deny, the caching grant wanted, durability, delete on close, `Backup` (the open asks for backup or restore intent: it is marked privileged when the caller is privileged, [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved), and otherwise opens unprivileged), create GUID and app instance, reclaim, the owner of opens and its sequence, a reconnect's match, a snapshot token, the parent and name it resolves through, `CreateArgs` for a create / the `OpenRef`, the `File`, `MaximalAccess`, the grant given, `Confirm` (the owner must confirm first, [RFC 14 §2.8](rfc-14-open-state.md#2.8%20Owner%20sequences)), `Created`, and the parent's `WCC` when it created |
| `AttrChange` | `metadata.Attrs` plus `Guard *time.Time` (a ctime guard, [RFC 20 §2.4](rfc-20-adapters.md#2.4%20Protocol%20details%20that%20stay%20in%20the%20adapter)), `DeleteOnClose *bool` and `Suspend`, `Resume` time masks — the last two only through an open |
| `Access` | an access mask in RFC 7's terms ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)) |
| `WCC` | `{Before *File; After File}`: one object's attributes either side of a mutation, from one hold of its accept lock; when `Before` is nil is [§5.7](#5.7%20GetAttr)'s |
| `CreateResult`, `UnlinkResult`, `RenameResult` | `{Handle; File; Dir WCC}`; `{Dir WCC}`; `{From, To WCC; Moved *WCC}` — `Moved` when the moved file's own attributes changed |
| `ShareRef`, `ShareInfo` | a share named as the client sent it — an NFS path or an SMB name; the zero value is the pseudo-root / a share's name, `Flavors []AuthFlavor`, comment, `Hidden`, the root's `MaximalAccess`, and the share policy's SMB flags (`ExportPolicy.SMB`: encrypt, continuously available, [RFC 16](rfc-16-metadata-store.md)) |
| `Change`, `Op` | one change a watch reports; the operation an event or metric names |
| `ClientInfo`, `ClientStatus`, `ClientRecord` | what `Propose` and `Connect` register — protocol, name, boot, whether state protection is on, the adapter's own bytes, and for `Connect` the `*Proposal` it confirms (nil for SMB and NLM, which confirm in one step); what `Renew` reports (§3.1); RFC 14's `Client`, aliased, as `Record` and `AddSession` return it |

The rest carry no meaning beyond their shape:

```go
type Protocol = protocol.Kind // NFS3, NFS4, NLM, SMB: the client record's kind (RFC 14 §2.1), one enum
type AuthFlavor uint8 // a flavour a share admits: sys, krb5, krb5i, krb5p, ntlm, none
type Stability uint8  // Unstable, DataSync, FileSync
type XattrMode uint8  // Create, Replace, Either
type SeekWhat uint8   // Data, Hole
type GrantKind uint8  // None, Read, ReadWrite, ReadHandle, ReadWriteHandle, an NFS delegation's read or write
type CreateKind uint8 // Dir, Symlink, Fifo, Socket, Char, Block, Regular

type CreateArgs struct { Kind CreateKind; Attrs Attrs; Verifier *[8]byte; Target []byte; Dev uint64 }
type WriteResult struct { N int; Verifier Verifier; WCC WCC }
type Extent = vfserr.Extent // { SrcOff, DstOff, Len int64 }, declared beside the errors (§4.3) so the engine needs no pkg/vfs
type CopyArgs struct { Src, Dst OpenRef; Extents []Extent } // Copy: in order, as copychunk's chunks; Clone: exactly one
type CopyResult struct { N int64; WCC WCC }
type LockReq struct { Ranges []LockRange; Wait WaitKind; Reclaim bool; Owner LockOwner; Replay *LockSequence; Seqs Seqs } // §5.11: every range in one step, all or none
type Proposal struct { Client ClientID; Confirm [8]byte } // what Propose answers and Connect confirms; no protocol sequence (RFC 14 §11.3)
type WatchSpec struct { Dir Handle; Recursive bool; Filter ChangeMask }
```

### 3.1 Operations

One interface per area, embedded in `Service`. An adapter is handed the whole
`Service`; a component that needs one area — an NLM or rquota handler, a
sidecar — takes that area's interface, and the conformance suite has one file
per area (§8).

```go
package vfs

type Service interface {
	Clients
	Names
	Attributes
	Data
	Locking
	Shares
	Principals
}

// Clients (RFC 14). The lease owner is the filesystem of the node holding the
// client's sessions; the gateway routes these calls to it (§2.1, §2.2), except
// TestState and FreeState, which go to the shard their StateRef names.
type Clients interface {
	Propose(ctx context.Context, c Call, info ClientInfo) (Proposal, error) // a two-step client's first call: RFC 14 §2.9's Client.Pending, held by this node's filesystem; changes no state
	Connect(ctx context.Context, c Call, info ClientInfo, cb Callbacks) (ClientID, error)      // SETCLIENTID_CONFIRM, the confirming CREATE_SESSION, SMB's first authenticated SESSION_SETUP (never a null or guest one), an NLM host's first call; for NFSv4.1, again on another node to re-register the back channel: the latest wins (RFC 14 Rebind); an SMB break never follows it (§3.2)
	AddSession(ctx context.Context, c Call, cl ClientID, prev, next []byte) (ClientRecord, error) // records this node's session and writes next as the record's adapter bytes if they still equal prev; otherwise writes nothing and returns the current record (RFC 14 §2.1)
	Record(ctx context.Context, c Call, cl ClientID) (ClientRecord, error) // the record as written, its adapter bytes included; changes nothing
	Renew(ctx context.Context, c Call, cl ClientID) (ClientStatus, error)
	Disconnect(ctx context.Context, c Call, cl ClientID, s SessionRef) error // an SMB session's last channel ended
	Destroy(ctx context.Context, c Call, cl ClientID) error                   // DESTROY_CLIENTID; StateError ClientBusy while held state remains, revoked references not counted
	ReclaimComplete(ctx context.Context, c Call, cl ClientID, shown []GraceRef) error // the instances ClientStatus.Reclaim showed it; ends only those
	FreeState(ctx context.Context, c Call, cl ClientID, s StateRef) error // a revoked reference, or a lock state holding no locks
	TestState(ctx context.Context, c Call, cl ClientID, s []StateRef) []error // TEST_STATEID: one answer per stateid, nothing changed
	ReleaseLockOwner(ctx context.Context, c Call, cl ClientID, o LockOwner) error // StateError LocksHeld while it holds a lock
	HostRestarted(ctx context.Context, c Call, host []byte, state uint32) error // NSM SM_NOTIFY, NLMPROC4_FREE_ALL: expires the host's old client
}

// Names. Every method also takes a pseudo-filesystem handle (§4.9).
type Names interface {
	Lookup(ctx context.Context, c Call, dir Handle, name []byte) (Handle, File, error)
	ReadDir(ctx context.Context, c Call, dir Handle, after Cursor, max int, want EntryFields) (Page, error)
	Create(ctx context.Context, c Call, dir Handle, name []byte, a CreateArgs) (CreateResult, error) // mkdir, symlink, mknod, NFSv3 CREATE
	Link(ctx context.Context, c Call, dir Handle, name []byte, target Handle) (CreateResult, error)
	Unlink(ctx context.Context, c Call, dir Target, name []byte) (UnlinkResult, error) // any entry; a directory only when empty
	Rmdir(ctx context.Context, c Call, dir Target, name []byte) (UnlinkResult, error)  // a directory only; ErrNotDir otherwise
	Rename(ctx context.Context, c Call, from Target, fromName []byte, to Handle, toName []byte, noReplace bool) (RenameResult, error)
}

// Attributes, ACLs, xattrs, streams.
type Attributes interface {
	GetAttr(ctx context.Context, c Call, h Handle) (File, error)
	SetAttr(ctx context.Context, c Call, t Target, a AttrChange) (WCC, error)
	GetACL(ctx context.Context, c Call, h Handle) (ACL, error)
	SetACL(ctx context.Context, c Call, t Target, acl ACL) (WCC, error)
	Xattrs(ctx context.Context, c Call, h Handle) ([]Xattr, error)
	SetXattr(ctx context.Context, c Call, t Target, x Xattr, m XattrMode) (WCC, error)
	RemoveXattr(ctx context.Context, c Call, t Target, name []byte) (WCC, error)
	Streams(ctx context.Context, c Call, h Handle) ([]File, error)
	Access(ctx context.Context, c Call, h, parent Handle, want Access) (Access, error) // Files.Granted ∩ want; parent zero when unknown
}

// Data.
type Data interface {
	Open(ctx context.Context, c Call, req OpenRequest) (OpenResult, error)
	Downgrade(ctx context.Context, c Call, ref OpenRef, q Seqs, access, deny Access) (uint32, error) // NFSv4 OPEN_DOWNGRADE: the open's new stateid seqid
	ConfirmOpen(ctx context.Context, c Call, ref OpenRef, q Seqs) (uint32, error)                    // NFSv4.0 OPEN_CONFIRM: the open's new stateid seqid
	Close(ctx context.Context, c Call, ref OpenRef, q Seqs) (*WCC, error)            // the parent's WCC when a delete on close ran
	Read(ctx context.Context, c Call, ref OpenRef, off int64, dst []byte) (n int, eof bool, err error)
	Write(ctx context.Context, c Call, ref OpenRef, off int64, src []byte, s Stability) (WriteResult, error)
	Commit(ctx context.Context, c Call, ref OpenRef, off, length int64) (Verifier, error) // NFS COMMIT, SMB FLUSH
	Deallocate(ctx context.Context, c Call, ref OpenRef, off, length int64) (WCC, error)
	Seek(ctx context.Context, c Call, ref OpenRef, off int64, what SeekWhat) (int64, error)
	Copy(ctx context.Context, c Call, a CopyArgs) (CopyResult, error) // COPY, copychunk: bounded chunks, synchronous, may be short
	Clone(ctx context.Context, c Call, a CopyArgs) (WCC, error)        // CLONE, duplicate-extents: atomic
}

// Locks, grants, watches (RFC 14).
type Locking interface {
	Lock(ctx context.Context, c Call, ref OpenRef, r LockReq) (StateRef, error) // the lock state's reference, new Seq included; zero where the protocol names none
	CancelLock(ctx context.Context, c Call, ref OpenRef, r ByteRange) (granted bool, err error)
	TestLock(ctx context.Context, c Call, ref OpenRef, r ByteRange, exclusive bool) (*Lock, error)
	Unlock(ctx context.Context, c Call, ref OpenRef, rs []ByteRange, q Seqs) (uint32, error) // in order; stops at the first range not held, ErrRangeNotLocked; the lock state's new seqid
	AckRecall(ctx context.Context, c Call, g GrantID, to GrantKind) error // to GrantNone: a return; StateError NoRecall: a lease or oplock with no recall in flight (RFC 14 §2.4)
	Watch(ctx context.Context, c Call, w WatchSpec) (WatchID, error)
	Unwatch(ctx context.Context, c Call, w WatchID) error
}

// Shares and volumes (§4.9).
type Shares interface {
	Root(ctx context.Context, c Call, s ShareRef) (Handle, ShareInfo, error) // MNT, PUTROOTFH, PUTPUBFH, TREE_CONNECT; zero ShareRef: the pseudo-root
	SecInfo(ctx context.Context, c Call, dir Handle, name []byte) ([]AuthFlavor, error)
	ListShares(ctx context.Context, c Call) ([]ShareInfo, error)
	StatFS(ctx context.Context, c Call, h Handle) (FilesystemInfo, error)                // the share's, or the tighter of it and h's project
	Quota(ctx context.Context, c Call, h Handle, p Principal) (Usage, Quota, error)      // p's own, else the share's default for its kind
	Snapshots(ctx context.Context, c Call, h Handle) ([]SnapshotInfo, error)             // the snapshots visible at h's path (RFC 12 §2.5)
}
```

```go
// Principals: sign-in and principal spellings. They act on no share and are
// not admitted; the filesystem delegates each to the identity component it holds
// (RFC 18 §3.1, §4.1). They run on the node holding the connection and are
// never forwarded.
type Principals interface {
	SignIn(ctx context.Context, step SignInStep) (SignInResult, error)           // one verification round: a ticket or an NTLM response in, the Identity, the session keys and the reply token out
	Spell(ctx context.Context, p Principal, as Scheme) (Spelling, error)         // owner string, SID, uid: RFC 18's spelling of p
	Find(ctx context.Context, c Call, s Spelling) (Principal, error)       // the principal holding s; mints a directory principal only for a proven identity
}
```

**Principals are not file operations.** Mapping a principal to a protocol's
spelling and back is
[RFC 18 §4.1](rfc-18-identity.md#4.1%20Two%20directions%2C%20one%20mapping)'s,
and verifying a handshake is
[RFC 18 §3.1](rfc-18-identity.md#3.1%20The%20authentication%20interface)'s;
the service exposes both as `Principals`, so an adapter imports `pkg/vfs` and
nothing else of the core. The resolver that maps an `Identity` per share stays
internal to the filesystem. `SignInStep`, `SignInResult`, `Scheme` and `Spelling`
alias RFC 18's types; `SignInResult` carries the session keys beside the
`Identity`, never inside it.

> decision: one interface per area, embedded, and no finer. Each area is one
> section of this RFC, one file of the conformance suite and the whole surface
> some consumer needs — NLM needs `Locking` and `Clients`, rquota `Shares` —
> so a test of that consumer fakes one area, not 50 methods. Rejected: one flat
> interface, which makes every narrow consumer declare its own copy of the
> methods it calls; and an interface per operation, which names groups nobody
> takes. Overturned if an area is found with no consumer that takes it alone.

**Every mutating call returns the attributes the reply needs**: the `WCC` of
every object it changed, captured as [§5.7](#5.7%20GetAttr) says, in the result
types of §3.0 (`CreateResult`, `UnlinkResult`, `RenameResult`, `WriteResult`,
`CopyResult`, `OpenResult`, and `Close`'s `*WCC`). An NFSv4 `change_info4` is the adapter's, derived from a directory
`WCC`'s change attributes and never reported atomic
([RFC 21 §9.2](rfc-21-nfs.md#9.2%20Close-to-open%2C%20and%20what%20the%20server%20guarantees)).
So no handler follows a mutation with a `GetAttr`
([RFC 20 §4.3](rfc-20-adapters.md#4.3%20A%20handler%20makes%20at%20most%20one%20mutating%20call)).

**Which calls change state.** These change nothing — no file, open state,
client record or lease — and a handler may make any number of them:
`Propose`, `Record`, `TestState`, `Lookup`, `ReadDir`, `GetAttr`, `GetACL`,
`Xattrs`, `Streams`, `Access`, `Read`, `Seek`, `TestLock`, `Root`, `SecInfo`,
`ListShares`, `StatFS`, `Quota`, `Snapshots` and `Spell`. Every other method may change
state, and RFC 20 §4.3 allows a handler one. `vfs.Mutates(op Op) bool` answers
it, so `vfstest.Record` and the adapters' one-call test read it from one place.
`Propose` is on the list because its record is volatile and nothing reads it
but the confirm. `Spell` is on it because an NFSv4 `GETATTR` asking `owner`
and `owner_group` makes two `Spell` calls and no mutation; `Find` is not,
since it may mint a directory principal. `Read` is on it although the store may advance access time;
that is a side effect of serving the read, never a second call.

> decision: the attributes are always returned, and atomic when present:
> `After` always, `Before` only as [§5.7](#5.7%20GetAttr) says. Rejected: a separate `GetAttr`, which is not atomic with the change and lets
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
call's `Identity` resolves to the principal the open was granted to, or to the
client's `Owner` for a call its state protection (`Protect`) names
([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)). A call by any other principal through
the same open or lock state **MUST** be refused with `NotYours`
([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), whatever that principal could
do to the file on its own: the state is the opener's, and a principal that wants
its own access opens the file itself.

**A delegation is the client's, not an opener's.** An NFSv4 client may do I/O
under its delegation's stateid for any of its open-owners and principals
(RFC 8881 §9.1.3), so an `OpenRef` naming a delegation is not bound to a
principal. The service **MUST** refuse it with `NotYours` unless the delegation
is held by the call's client on a session (`Call.Session` set); a `Delegated`
reference on a call with no session is `NotYours`, since NFSv4.0 is offered no
delegation. The service **MUST** then authorise the call as for an
anonymous open: the `Identity`'s principal against the file
and the share grant ([§4.6](#4.6%20One%20chokepoint)), and against the deny
modes held. The delegation lends no access.

> decision: `Unlink` removes any entry, an empty directory included, and `Rmdir`
> only a directory. NFSv4 `REMOVE` and an SMB delete name an entry without its
> type, and a handler that looked the type up first would race the entry it
> removes; NFSv3 `RMDIR` is the one request that must refuse a non-directory.
> Split further only if a protocol is found that must refuse a directory on
> its remove.

> decision: there is no space reservation, so `Allocate` is not offered. NFS
> `ALLOCATE` is answered not supported. An SMB allocation size below the end of
> file truncates the file, as a size change does
> ([RFC 22 §9.3](rfc-22-smb.md#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files));
> one at or above it is accepted and changes nothing but what the adapter
> reports. A reservation would promise
> that a later write cannot fail for space, which a journal shared by many shares
> and drained by offload cannot keep. Add one, as a per-share charge against the
> journal's capacity, if a workload is shown to need `ALLOCATE`'s guarantee.

> decision: no pre-warm. Fetching a tree ahead of its readers is an
> operator's action, one of [RFC 23](rfc-23-control-plane.md)'s management
> operations over the engine's `PreWarm`, not a client's. pNFS layouts are
> §10's second open question.

### 3.2 Callbacks

```go
// Callbacks is implemented by each adapter and registered per client at
// Connect. The service calls up through it and never imports an adapter.
type Callbacks interface {
	RecallGrant(ctx context.Context, s SessionRef, g CachingGrant, to GrantKind) error // NFS CB_RECALL on the client's back channel, s zero; SMB lease or oplock break on s, which the service picked and the adapter only delivers to
	Notify(ctx context.Context, w WatchID, changes []Change) error      // SMB CHANGE_NOTIFY, NFS directory notifications
	Revoked(ctx context.Context, what Revocation)                       // state ended without the client asking: the adapter answers what it holds pending on it
	LockWaitOver(ctx context.Context, w LockWaiter, granted bool) (refused bool, err error) // NLM_GRANTED, SMB lock completion (granted); NFSv4.1 CB_NOTIFY_LOCK, optional; refused: the NLM host refused the grant, and the primary releases the lock
	RecallAny(ctx context.Context, keep int) error // NFSv4.1 CB_RECALL_ANY for read and write delegations; nil and no call for SMB and NFSv4.0 (RFC 14 §5.5)
}

// Revocation and RevokeReason are RFC 14's (§11.1), aliased in pkg/vfs; shown
// here because the adapters act on them.
//
// Revocation names state the service ended without the client asking. Share
// is set for RevokedShare, where only that share's state ends; Grant for
// RevokedGrant; Shard for RevokedShardState (RFC 14 §4.4). RevokedLeaseExpired
// and RevokedByAdmin end every piece of state the client holds (RFC 14 §4.3,
// §5.3). Until the client frees what was revoked (FreeState), Renew reports
// it by cause in ClientStatus, and a call naming it is refused ErrState (Revoked).
// A removed or moved share's state is the exception: DropShare deletes the
// records that could list it, so a call naming it is StateError BadState and
// no flag is set, since nothing could ever clear one (RFC 16 §2.3.2, RFC 14).
type Revocation struct {
	Client ClientID
	Share  ShareID
	Grant  GrantID
	Shard  ShardID
	Reason  RevokeReason
	Addr    netip.Addr // RevokedShare: non-zero when the share moved and the move names an address (RFC 27 §2.2)
	Adapter []byte     // the client record's adapter bytes, so the adapter can act with no read: an NLM host's notice address (RFC 14 §4.5)
}

type RevokeReason uint8

const (
	RevokedLeaseExpired RevokeReason = iota // the client's lease expired
	RevokedByAdmin                          // an administrator revoked the client; its record and sessions are kept (RFC 14 §5.3)
	RevokedShare                            // RFC 14 DropShare: the share was removed, or moved away when Addr or the move says so
	RevokedGrant                            // one caching grant whose recall missed its deadline
	RevokedShardState                       // a shard's open state was lost
)
```

`ClientStatus` is what `Renew` reports: `ReclaimNeeded` (a shard of the client's
state restarted its grace) with `Reclaim`, the (shard, grace instance) pairs it
owes, which the adapter keeps per session and hands back to `ReclaimComplete`
([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)), `AdminRevoked` and `RecallableRevoked` (a stateid of
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

![A recall without a waiting worker: build01's NFS write conflicts with alice-pc's SMB lease, so the service sends RecallGrant, which the SMB adapter turns into a lease break, and answers ErrDelay at once, mapped to NFS4ERR_DELAY or NFS3ERR_JUKEBOX; alice-pc acknowledges, the service gets AckRecall, and build01's retry goes ahead. If alice-pc never answers, the break is forced at 35 s and the lease drops to none; if the conflicting caller is an SMB client, the SMB adapter sends an interim response and re-drives the call itself, held up to smb_pending_deadline, which exceeds the break deadline](img/rfc17-recall.svg)

**No worker waits on a client.** An operation that conflicts with a caching
grant starts the recall and returns `ErrDelay` at once. An NFS adapter answers
the protocol's retry signal and the client retries. SMB has no retry signal —
its client does not resend a request answered with an interim response — so
the SMB adapter holds the request and re-drives it itself, a mechanism of its
request pipeline ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)).
The service's one wait inside a call is behind a running clone or copy chunk,
bounded by the hold the adapter allows (`vfs.WithHold`, [§5.9](#5.9%20Copy%20and%20clone)).

> decision: the service classifies and the adapter holds. The service names a
> delay's cause and bounds only its own wait behind a clone;
> the adapter's pipeline (`core/hold.go`) holds and re-drives — `STATUS_PENDING`,
> a timer, no worker, the resolve and call run again, never in a handler — and
> RFC 24's hold pool bounds the bytes held. Rejected: the service parking wire
> requests, which cannot send `STATUS_PENDING`; per-handler re-drive, which is
> today's and drifts per command; and a hold-policy hook in the context, which
> made the service count what only the adapter holds. Overturned if a protocol
> needs a hold the service alone can see the end of.

**How long a delayed request may be held** is the adapter's, chosen by the
`Delay`'s cause and never by the operation: an NFS adapter answers its retry
signal at once, and SMB's bounds and final statuses are
[RFC 22 §16.5](rfc-22-smb.md#16.5%20Errors)'s. Two
bounds there follow from this RFC and **MUST** hold: a request held behind a
recall or a clone is held past the break deadline —
[RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)'s
35 s for SMB — so it outlives the recall it waits on, and one held behind a
freeze past `migration.freeze_timeout` plus the break deadline
([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)).
[RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)
refuses settings that break either. `ErrBlocked` holds no worker and has no
deadline: it ends only as its waiter does
([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)), and an adapter
that answers it other than as granted calls `CancelLock` first.

**Recalls after one is revoked.** After one of a client's recalls is revoked, the
primary **MUST** stop offering that client grants for one lease period; the
others it holds end by their own deadlines when something conflicts
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

**An NFSv4.1 callback path can move between nodes** (cluster, [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)).
An NFSv4.1 client's callbacks go out through the node that holds its back
channel; a primary on another node reaches it through `Peers.Recall` (§2.3).
When the client binds its callback path through another node — after an
address takeover, or a lost connection — that node's adapter calls `Connect`
again, and its filesystem records it in the client's record (RFC 14's `Rebind`); the
latest wins, and a recall in flight is retried through it. Without the record,
every recall after the client moved would go out through a path the client no
longer listens on and end in revocation.

**Only the client's own principal moves its path.** The filesystem **MUST** refuse
with `StateError` `ClientInUse` an NFSv4 `Connect` that would move an existing
client's callback path for a caller whose authenticated principal is neither
the one the client record names nor one holding a session under that client
([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)). An SMB
`Connect` is never refused for its principal, because its breaks go by open
(below), not by a path a `Connect` could move
([RFC 22 §4.4](rfc-22-smb.md#4.4%20The%20client%20record%20is%20the%20machine)).

**An SMB break goes by its open, not by the client's path.** An SMB record names
no principal, and its `ClientGuid` travels in clear, so anyone can send a
machine's and hold a session under its record
([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)). An SMB client is
registered at its first authenticated `SESSION_SETUP`, never at `NEGOTIATE`,
and a null or guest session never joins a client, so its opens are offered no
lease or oplock. The filesystem picks the session for a lease or oplock break:
one holding one of the grant's opens, each signed in as its opener; when every
such open is disconnected, only a session of that client signed in as one of
the openers; and when there is none, none, so the break is sent nowhere and
runs to its deadline. It passes that session to `RecallGrant`, through
`Peers.Recall` to the node the `SessionRef` names when it is another, and the
adapter delivers there and nowhere else. `AckRecall` on a lease or oplock is
admitted only from one of its openers' principals, and an open joins an
existing lease only for one of them or for a caller on the same connection as
an opener's session
([RFC 14 §2.4](rfc-14-open-state.md#2.4%20CachingGrant)). So a peer presenting
another machine's `ClientGuid` and lease key, through this node or a second
one, receives none of its breaks, and the machine's leases are not revoked for
breaks sent elsewhere.

**Notifications honour traverse permission.** `Notify` delivers a change only
when the watch's identity may traverse every directory between the watched one
and the change, evaluated through the chokepoint ([§4.6](#4.6%20One%20chokepoint))
at delivery. A recursive watch never reports names below a directory its holder
cannot enter.

### 3.3 Event hooks

```go
// Events is implemented by an event sink the node supplies. The chokepoint
// emits nothing (RFC 7 §7.6); the filesystem builds and emits every event, and
// never waits on the sink.
type Events interface {
	Quota(ctx context.Context, e QuotaEvent)   // advisory threshold crossed, soft limit exceeded, grace expired, hard refusal, back under
	Access(ctx context.Context, e AccessEvent) // one authorisation decision the audit policy selects
}

type AccessEvent struct {
	Time       time.Time
	Share      ShareID
	Client     ClientID   // zero for NFSv3
	Addr       netip.Addr // the client's address
	Identity   Identity
	Protocol   Protocol
	Op         Op
	File       FileID
	Name       []byte // the name acted on, for namespace operations
	Want       Access
	Allowed    bool
	SelectedBy Selector // Policy: the share's audit policy; Entry: an audit or alarm entry of the file's ACL, which the chokepoint reports with its decision (RFC 7 §7.6, RFC 19 §3.7)
}

// QuotaEvent is one quota transition or refusal. Transitions are emitted once
// each; hard refusals are coalesced by the sink per share, scope, subject and
// resource (RFC 25 §4.4), which stamps the count it coalesced.
type QuotaEvent struct {
	Time      time.Time
	Share     ShareID
	Scope     QuotaScope         // User, Group, Project or Share
	Subject   Principal          // the user or group; zero for a project or share quota
	Project   metadata.ProjectID // the tree; zero otherwise
	Resource  QuotaResource      // Bytes or Files
	Kind      QuotaEventKind     // AdvisoryCrossed, SoftExceeded, GraceExpired, HardRefused, BackUnder
	Used      int64              // the usage judged, in the resource's unit
	Soft      int64
	Hard      int64
	GraceEnd  time.Time          // zero when no grace runs
	Writer    Identity           // the refused caller, for HardRefused only
}
```

`QuotaEvent` names the quota, the principal or project, and what happened:
advisory threshold crossed, soft limit exceeded, grace expired, hard refusal,
back under. Both kinds are emitted from the one path every operation takes, by
the filesystem and nothing else. **The chokepoint emits nothing**
([RFC 7 §7.6](rfc-7-namespace-metadata.md#7.6%20Decisions%20are%20observable)):
`Files.Authorize` returns its decision and whether an audit or alarm entry of
the file selected it — only the chokepoint knows that — and never reads the
share's audit policy. The filesystem applies the policy and builds the access event from the resolved caller, which carries the protocol and
client, and the file, access and outcome, when the share's audit policy selects
the decision or honours the entry (RFC 19 §3.7). Quota events are emitted where
the filesystem enforces quota ([§5.6](#5.6%20Quota)). Each decision is audited
once, and no adapter, store or engine emits either, so no protocol can reach a
file without appearing in the audit stream.

- An event hook **MUST NOT** block or fail the operation. The sink queues, and
  an event it cannot queue is dropped and counted by the sink (RFC 25's
  `events_dropped_total`).
- Which operations produce an access event — every decision, denials only,
  writes and namespace changes, per share — and whether the file's audit
  entries select events too (RFC 19 §3.7) is the audit policy, a setting
  ([RFC 13](rfc-13-configuration.md)). With no policy, no access event is built, whatever audit entries
  a file holds.
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
| `Handle` | a file, or a pseudo directory | the metadata store's handle codec ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)) | opaque: the share and the `FileID`, never a shard, path or name; a pseudo directory's is its own kind, a digest of the installation and the path | yes | the primary's resolve: `ErrStale` ([RFC 7 §6.3](rfc-7-namespace-metadata.md#6.3%20Staleness%20is%20reported%2C%20never%20guessed)) |
| `Cursor` | where a listing resumes | RFC 7's entry digest ([RFC 7 §3.5](rfc-7-namespace-metadata.md#3.5%20A%20cookie%20survives%20concurrent%20mutation)) | 63 bits, top bit clear, 0–2 reserved; the cookie verifier is a constant of the share ([RFC 21 §7.2](rfc-21-nfs.md#7.2%20READDIR%20cookies%20and%20verifier)) | yes | the listing: a resume past a removed entry still resumes |
| NFSv4 stateid | an open, lock state, delegation, layout or copy | RFC 14 ([RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references)) | `seqid` 4 bytes; `other` 12: byte 0 the type tag (low 3 bits) and the grace instance's low 5 bits, bytes 1–3 the shard number, bytes 4–11 the record's 64 random bits ([RFC 21 §5.2](rfc-21-nfs.md#5.2%20Stateids)) | yes; lost state is told by the grace bits | the primary: `StateError` `StaleState` or `BadState` |
| anonymous and special stateids | the anonymous open; the current stateid | the protocol | all zeros, all ones, NFSv4.1's current | — | the adapter resolves "current" within the compound; all ones is anonymous for `READ` only |
| SMB `FileId` | an open | RFC 14's `OpenID` | persistent half: the open's shard and its `OpenID` ([RFC 22 §6.1](rfc-22-smb.md#6.1%20Opening%20a%20file)); volatile half: the table entry of the node holding the connection, the adapter's alone | the persistent half yes; the volatile half no | the primary, for the persistent half; the adapter, for the volatile |
| `ClientID` | a client | RFC 14, from the client record | 8 bytes, unique and never reused, not random ([RFC 21 §5.1](rfc-21-nfs.md#5.1%20Client%20ID%2C%20lease%20time%2C%20expiry)) | yes | the lease owner: `StaleClient` |
| `SessionRef` | an SMB session | the node holding the session | (node, node epoch, session ID) ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)) | no: a new session after a restart | — |
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

The stateid's and the `FileId`'s widths — a 24-bit shard number, 5 grace-instance bits, a 40-bit `OpenID` — are one choice, [RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references)'s. Only an SMB, durable or clone source open takes an `OpenID`; an NFS open is named by its stateid's token ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)).

### 3.5 What each method calls

Every service method, what it calls below, and where it runs: "primary" is the
primary of the handle's shard, "coordinator" is the filesystem at
[RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries)'s
coordinator, and "lease owner" is the local filesystem, as
[§2.1](#2.1%20What%20runs%20where) routes them. A step at a second primary goes
through `Peers` (§2.3). Gaps in the layers below are rows of §10's fourth. Which wire request becomes which method is the protocol RFCs':
[RFC 21 Appendix D](rfc-21-nfs.md#Appendix%20D%20%E2%80%94%20requests%20and%20the%20service%20calls%20they%20make)
and [RFC 22 Appendix G](rfc-22-smb.md#Appendix%20G%20%E2%80%94%20commands%20and%20the%20service%20calls%20they%20make).
An open-state call is named by its RFC 14 view (§2.3); `openstate.Clients` is
qualified so as not to read as the store's `metadata.Clients`, and a bare
`Opens` is RFC 14's view too, never the store's `metadata.Opens`
([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)).

| Method | Metadata calls (RFC 7, RFC 6, RFC 14, RFC 16, RFC 12) | Engine calls (RFC 8) | Runs at |
| --- | --- | --- | --- |
| `Propose` | `openstate.Clients.Propose`, which looks the (protocol, name) up through the table, never the store's `metadata.Clients`: an existing client's ID, or `ErrClientInUse` for another principal ([RFC 14 §2.9](rfc-14-open-state.md#2.9%20Clients%20pending%20confirmation)) | — | lease owner; it holds the `Client.Pending` proposal in memory, so the confirm must reach the same node |
| `Connect`, `AddSession`, `Renew`, `Destroy` | `openstate.Clients.Connect` (and `Rebind`), `AddSession`, `Renew`, `Destroy` | — | lease owner, which keeps the callback registry |
| `Disconnect`, `ReclaimComplete`, `ReleaseLockOwner`, `HostRestarted` | `openstate.Clients.Disconnect`, `Recovery.ReclaimComplete`, `Locks.ReleaseLockOwner`; `openstate.Clients.Connect` under a new NSM state for `HostRestarted` | — | lease owner, then each primary holding the client's state, through `Peers.ClientState` |
| `TestState`, `FreeState` | `Recovery.TestState`; `Recovery.FreeState`, which also frees a lock state holding no locks | — | the primary of the `StateRef`'s shard; `TestState`'s other refs through `Peers.ClientState` |
| `Lookup` | `Namespace.Lookup` (authorised inside, [RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)) | `Overlay` | the directory's primary; the entry's primary for the overlay, through `Peers.Overlay` |
| `ReadDir` | `Namespace.Entries` or `EntriesPlus` | `Overlay` per entry, batched per primary through `Peers.Overlay` | the directory's primary |
| `Create`, `Link` | `Namespace.Create` / `Link`, `Checks.CheckChange` on the parent | `Draw` (parent, new file) | coordinator; each other primary through `Peers.Prepare` and `Outcome` |
| `Unlink`, `Rmdir`, `Rename` | `Namespace.Lookup`, `Files.Authorize`, `Checks.CheckChange` on the file, a replaced target and the destination parent, `Namespace.Unlink` / `Rename`; `Opens.Renamed` after a rename | `Draw`; `Delete` for an orphan | coordinator; each other primary through `Peers.Prepare` and `Outcome` |
| `GetAttr` | `Files.Get` | `Overlay` | primary |
| `SetAttr` | `Files.SetAttrs`, `Files.Authorize`; `Checks.CheckChange` through the target's open, `ChangeSetID` for a mode setting setuid or setgid; `Opens.Update` for delete on close and suspended times | `Draw`; `Truncate` for a size | primary |
| `GetACL`, `Xattrs`, `Streams` | `Files.ACL`, `Xattrs`, `Streams`, authorised inside ([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)) | — | primary |
| `Access` | `Files.Granted`, through `authz` (§4.6) | — | primary |
| `SetACL`, `SetXattr`, `RemoveXattr` | `Files.SetACL`, `SetXattr`, `RemoveXattr` | `Draw` | primary |
| `Open` | `Namespace.Lookup` or `Create`, `Files.Authorize`, `Granted`, `Opens.Open` or `Reconnect` | `Draw` when it creates | primary ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| `Downgrade`, `ConfirmOpen` | `Opens.Downgrade`, `ConfirmOpen` | — | primary |
| `Close` | `Opens.Close`; `Namespace.Unlink` for a delete on close | `Delete` for an orphan | primary ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| `Read` | `Checks.CheckIO`; `Files.Authorize` for an anonymous or delegated open | `Read` | primary |
| `Write` | `Checks.CheckIO`; `Files.Authorize`; `Files.SetAttrs` to clear setuid; `Capacity.Usage` for quota | `Draw`, `Write`, `Overlay`, `Commit` when stable | primary |
| `Commit` | `Opens.Flushed` through an SMB open | `Commit` | primary |
| `Deallocate`, `Seek` | `Checks.CheckIO` | `Deallocate`; `Allocation` | primary |
| `Copy`, `Clone` | `Checks.CheckIO` on both | `Copy`, `Clone`; across shards `Read` and `Write` | the destination's primary, reading at the source's across shards through `Peers.ReadAt` |
| `Lock`, `CancelLock`, `TestLock`, `Unlock` | `Locks.Lock`, `CancelLock`, `TestLock`, `Unlock` | — | primary ([§2.2](#2.2%20Who%20owns%20open%20state%20and%20locks)); an NFSv4.0 owner's sequence at its home primary through `Peers.Sequence` |
| `AckRecall` | `Grants.AckRecall`, `GrantNone` included | — | the primary of the shard the `GrantID` names |
| `Watch`, `Unwatch` | `Watches.Watch`, `Unwatch` | — | the directory's primary; `Unwatch`, the primary of the shard the `WatchID` names |
| `Root`, `SecInfo`, `ListShares` | `ShareIndex`: share records, export policy and the name and path indexes ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)); `Files.Root` | — | lease owner |
| `SignIn`, `Spell`, `Find` | RFC 18's authenticator and principal mapping, held by the filesystem ([RFC 18 §3.1](rfc-18-identity.md#3.1%20The%20authentication%20interface), [§4.1](rfc-18-identity.md#4.1%20Two%20directions%2C%20one%20mapping)) | — | the local filesystem; never forwarded |
| `StatFS`, `Quota` | `Capacity.Info`, `Capacity.Usage`; the quota record (gap: no view but `ControlPlane.Quotas`) | — | primary |
| `Snapshots` | RFC 12 `Snapshots.List`, filtered by the handle's path | — | primary |

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
adapter's rule, kept in the client record's adapter bytes and written through
`AddSession`, unlike a session's reply cache, because the client record it
belongs to is shared across nodes
([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)).

### 4.3 Errors are neutral values

The service returns the errors below. They are declared once, in
`pkg/vfs/vfserr`, a leaf package that imports only the standard library: every
sentinel and typed kind, all of `DelayCause` and `CrossKind`, and `Extent`. The
engine returns `ErrDelay` with its causes and `ErrCross` with its kinds, and Go
cannot split an enum across two packages, so each enum moves whole, values only
the service returns included. `Extent` moves with them because while
`vfs.Extent` aliased the engine's, every adapter importing `pkg/vfs` compiled
the engine. So the engine imports `vfserr` and never `pkg/vfs`, nor open state
through it, and `pkg/vfs` re-exports every value as an alias. `vfs.Errors()`
stays in `pkg/vfs` and returns every sentinel and one value of every typed
kind, so an adapter's error table can be checked total
([RFC 20 §7.3](rfc-20-adapters.md#7.3%20The%20error%20tables%20are%20total)).
How each reaches the wire is
[RFC 21 §11.5](rfc-21-nfs.md#11.5%20Errors),
[RFC 22 §16.5](rfc-22-smb.md#16.5%20Errors) and the
adapters' tables ([RFC 20 §4.5](rfc-20-adapters.md#4.5%20One%20error%20table%20per%20protocol)).

| Error | Means | Kind |
| --- | --- | --- |
| `ErrNotFound`, `ErrExist`, `ErrNotEmpty`, `ErrNotDir`, `ErrIsDir` | the name or entry is not as the call needs | — |
| `ErrAccess` | the share grant, its client rules, or the file's mode or ACL refuse the caller; or the read-only marker refuses a write or a delete ([RFC 7 §7.9](rfc-7-namespace-metadata.md#7.9%20The%20read-only%20marker%20refuses%20writes%20and%20deletes)) | `*AccessError{Cause}`: `ReadOnly` when the read-only marker refused a delete; absent otherwise |
| `ErrPerm` | refused whatever the caller's access: an immutable or append-only file ([RFC 7 §7.8](rfc-7-namespace-metadata.md#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint)), or a change of owner, group or flags only a privileged caller or the owner may make ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)) | — |
| `ErrStale` | a handle to nothing, or to a share being removed or retired ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)) | — |
| `ErrReadOnly` | a mutation where only reading is admitted: a share or a client rule narrowed to read-only ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)), a snapshot ([RFC 12](rfc-12-snapshots.md)), the pseudo-filesystem | — |
| `ErrNameTooLong` | a name past the share's length limit ([RFC 7 §3.2](rfc-7-namespace-metadata.md#3.2%20A%20name%20is%20bytes%2C%20and%20it%20is%20validated%20at%20the%20boundary)); any other name the store refuses is `ErrInvalid` | — |
| `ErrInvalid` | the arguments cannot be honoured as given: a name the store refuses, an overlapping copy or clone within one file, a clone longer than `clone_max_len`, a deny entry naming a well-known principal [RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)'s table lacks | — |
| `ErrNoSpace` | a permanent capacity refusal: no space comes without operator action ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s second row); answered at once | — |
| `ErrQuota` | a logical quota refused; answered at once | `*QuotaError{Scope}`: `QuotaShare`, `QuotaProject`, `QuotaUser`, `QuotaGroup`, the first exceeded in that order — four, though adapters map only two classes, because the metric label and the refusal's message name the scope |
| `ErrLocked` | a byte-range lock others hold conflicts | — |
| `ErrRangeNotLocked` | an `Unlock` named a range the owner does not hold | — |
| `ErrShareViolation` | a deny mode others hold conflicts, decided after handle caching is broken ([§5.3](#5.3%20Open%20and%20close)) | — |
| `ErrDeletePending` | the file is marked delete on close ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)) | — |
| `ErrGrace` | the shard is in grace and the request needs, or conflicts with, state that may still be reclaimed | — |
| `ErrDelay` | retry shortly | `*Delay{Cause; Until; Moved}`, below |
| `ErrBlocked` | a lock waits, and will be granted through `LockWaitOver`: the service's answer for RFC 14's `LockResult{Waiting: true}` | — |
| `ErrWrongSecurity` | the share's policy does not admit the call's flavour ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)) | — |
| `ErrState` | a client-held name of state cannot be used as given | `*StateError{Kind}`, below |
| `ErrCross` | the call spans what cannot be spanned | `*CrossError{Kind}`: `CrossNamespace` (a copy or clone between namespaces), `CrossShare` (a rename or link between shares, [RFC 7 §5.1](rfc-7-namespace-metadata.md#5.1%20One%20transaction)), `CrossProject` (a rename or link across tree quotas, [RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)), `OtherShard` (**(cluster)** a clone whose source and destination lie in different shards, [§5.9](#5.9%20Copy%20and%20clone)) — the first three name the boundary a client can see; `OtherShard` names placement, which a client cannot, so adapters fall back rather than report it |
| `ErrIncomplete` | a read of a file a snapshot lists as incomplete, or a copy or clone from one ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) | — |
| `ErrInconsistent` | a store invariant the filesystem's own state breaks; the store raises a health condition naming the record ([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)) | — |
| `ErrLost`, `ErrUnavailable`, `ErrCorrupt` | the content errors of [RFC 8](rfc-8-engine.md) | — |

`StateError` kinds, each matched by `errors.Is(err, ErrState)`, are RFC 14's
refusals: `ClientInUse` (a client name another principal holds), `ClientBusy`
(`Destroy` with state left), `RecordChanged` (a client record's adapter bytes
changed since the adapter read them: reread and decide again), `AdapterVersion`
(adapter bytes of a version this node cannot read), `StaleClient` (a client
that expired, or is marked by a shard's loss until it proposes and confirms
again, [RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)),
`StaleState` (a stateid or `FileId` whose state may have been lost with its
shard's open state, by the grace-instance rule of §3.4), `OldState` (a stateid
seqid older than the state's current one), `BadState` (every
other stateid or `FileId` whose record is missing — expired, freed, a removed or
moved share's, never issued), `NoReclaim` (a reclaim in a shard the client's
record does not list, an administrator's revoke having emptied it included),
`ReclaimConflict` (a reclaim that conflicts with one already reclaimed in the
same grace), `LocksHeld` (a lock owner released, or a lock state freed, while
it still holds locks; a live open or grant freed), `BadSeqID` (an
NFSv4.0 owner sequence out of order), `NoRecall` (an acknowledgement of a lease
or oplock with no recall in flight), `CreateInUse` (an SMB create naming a create
ID another open of its client holds, [RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)), `NotYours` (state another client or
principal holds, §3.1) and `Revoked` (the call names state in its client's
revoked set, not yet freed; `Admin` tells an administrator's revoke from a
recall missed).

```go
type Delay struct {
	Cause DelayCause
	Until time.Time  // DelayFrozen: the freeze's bound; zero otherwise
	Moved bool       // DelayFrozen: the share's namespace has moved away; the address is the RevokedShare revocation's Addr
}

type DelayCause uint8

const (
	DelayRecall    DelayCause = iota // a caching grant is being recalled (§3.2)
	DelayClone                       // a running clone or copy chunk holds the extent past the call's hold (§5.9)
	DelayConflict                    // a metadata conflict still unresolved at the call's deadline (RFC 0 §9.2), or an open-state owner's own numbered request in flight (RFC 14 §2.8)
	DelayFrozen                      // the share is quiesced, frozen for a move, or moved away; or an open-state shard not yet served here (RFC 14 §11.3)
	DelayCapacity                    // the journal's capacity or a share's journal limit (RFC 8 §10.2)
	DelayCommit                      // a metadata commit is not landing: a stalled store, the version limit's raise, or (cluster) quota slack spent (RFC 8 §11.2, §5.6)
	DelayResources                   // a node memory pool is spent: the share cap or the open-state pool; an adapter builds the same for a reply reservation past its deadline (RFC 24 §4.2)
)
```

`Cause` labels metrics and selects how long an adapter holds the request and
its final status (§3.2). **A transient refusal never becomes `ErrNoSpace` because a
deadline ran out**: a capacity refusal still `ErrDelay` at the caller's
deadline is answered `ErrDelay`, and only [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s
permanent causes are `ErrNoSpace`. A conflict the service still meets at the
call's deadline, its retries spent, is `DelayConflict`, never an I/O error
([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)).
A routing refusal — wrong primary, stale epoch — **MUST NOT** reach an adapter:
the gateway re-routes and retries within the caller's deadline
([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20primaries)).

**Every call carries the pipeline's per-attempt deadline**
([RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits)); the service
sets none. A held request's total time is its cause's hold bound, the
adapter's (§3.2).

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
([RFC 7 §7.1](rfc-7-namespace-metadata.md#7.1%20One%20chokepoint)). The service's
`authz` package calls it, and `Files.Granted`, on every operation that needs
them, and is their only caller. An open-time grant ([RFC 7 §7.2](rfc-7-namespace-metadata.md#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always))
is computed there and stored with the open; the filesystem evaluates it on
the operations it gates. Every call runs admission, then authorisation, then the
conflict check, in that order:

```go
// The order of every call (indicative), in the filesystem at the file's primary,
// where the gateway has routed it (§2.1); RFC 7 §7 owns what Authorize decides.
caller, err := s.admit(ctx, call, shareOf(h)) // state, flavour, client rules, resolution, share cap → RFC 18's Caller
switch {
case t.Open != nil && t.Open.Kind == Opened: // through an open: its stored grant
	g := openGrant(t.Open)                     // RFC 14's Access, Privileged and Via
	if !samePrincipal(caller, g) { return StateError{NotYours} }
	err = meta.Files.Authorize(ctx, caller.With(g), h, g.Via, want)
default: // anonymous, delegated or no open: the caller afresh
	err = meta.Files.Authorize(ctx, caller, h, parent, want) // parent: the directory h was resolved through, or zero
}
// the chokepoint has emitted the access event the audit policy selects (§3.3)
err = open.CheckIO(ctx, keyOf(f), ref, r, write)  // or CheckChange: deny modes, locks, grants — RFC 14 §7
```

- **The share grant is checked on every call**, not only at `Root`. The
  filesystem reads the caller's grants at admission each time (§4.9 step 4),
  `Authorize` applies the access they give as the share ceiling, and any cache
  of authorisation results **MUST** include the share's grant-list version,
  which a grant's deletion moves as well as its change, and the parent directory with its whole record's version — owner and group as well
  as mode and ACL, since the group bits and the sticky rule read them — in its
  key
  ([RFC 7 §7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read)).
  A client removed from a share loses access on its next call, cached handles
  or not.
- **An open is decided with its parent.** The service passes `Authorize` and
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
  privileged (an open with `OpenRequest.Backup` set by a caller privileged
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

The gateway forwards a `Service` call as it is, so every `Service` method takes
and returns values, holds no pointer into the caller, and resumes a listing
from a cursor the caller holds. This holds on a single node too, so putting a
gateway between the adapters and the filesystem changes no signature. `Callbacks` is the one
interface argument, and `Connect` runs at the lease owner, the local filesystem, so
it never crosses. Every forwarded call carries
[RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)'s route
envelope, whose dedup table returns a mutation's original result to a retry.
Repeating a mutation with the same arguments is **not** assumed to be safe. An
adapter names each call's request ID with `vfs.WithRequestID(ctx, id)`, built
from the protocol's retry identity as RFC 15 §4.3 says, and the gateway copies
it into the envelope.

`ReadDir` returns one page and the cursor after it; the filesystem fills it by
asking the store for entries after the caller's cursor, so no iterator state
lives on any node between calls. An adapter fits the page to its reply
([RFC 20 §4.9](rfc-20-adapters.md#4.9%20Sharing%20code%20across%20versions%20and%20protocols))
and resumes from the cursor of the last entry it sent.

### 4.8 One primary per file

Every call that changes a file, or checks open state on it, runs at the primary
of the file's shard ([RFC 11](rfc-11-ownership.md)), which holds the file's
namespace records, its open state and its content. The gateway only forwards. The
conflict check and the I/O it gates therefore run at one primary, with no hop
between them. The primary **MUST** order them so that no lock, deny mode or
grant is granted between the check and the I/O it admits: a conflicting grant
is ordered wholly before the check, which then refuses, or wholly after the I/O.

Operations that touch two primaries are
[RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries)'s,
run by its coordinator, the filesystem at the source directory's primary; a
cross-shard copy and clone are
[§5.9](#5.9%20Copy%20and%20clone)'s.

### 4.9 Shares, mounts and trees

**Admission runs on every call.** Before anything else — before the handle is
resolved, since a handle names its share (§3.4), so a client refused admission
learns nothing of the file it names — the filesystem admits the call to the share of
the handle or open it names, in this order:

1. the share's state ([RFC 16 §2.3.1](rfc-16-metadata-store.md#2.3.1%20Share%20names%2C%20paths%20and%20state)) — `ErrDelay` (`DelayFrozen`) while quiesced,
   `ErrStale` once being removed or retired ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share));
2. the share's `ExportPolicy` minimums, `MinProof` and `MinProtection`, against the `Identity`'s `Proof` and `Protection` — `ErrWrongSecurity`;
3. its client rules, against the call's address and the netgroups it is in —
   `ErrAccess`, or a narrowing to read-only — and the policy's `ReadOnly` flag,
   which narrows every caller to read-only;
4. resolution, which turns the `Identity` into the caller it acts as on this
   share: map it to a principal, squash it, collect its groups, read its share
   access from the grants, and set its flags
   ([RFC 18 §6.1](rfc-18-identity.md#6.1%20From%20Identity%20to%20caller)).
   Step 3's narrowing to read-only, by a client rule or `ReadOnly`, reaches
   the chokepoint as a field of that caller, since the chokepoint evaluates no
   client rule (RFC 19 §3.2);
5. the share's cap on the node's request buffers
   ([RFC 24 §4.2](rfc-24-resources.md#4.2%20Across%20shares%3A%20the%20share%20cap%20at%20admission))
   — `ErrDelay` with `DelayResources`, at once — except a call that only
   releases state (close, unlock, lock cancel, grant or layout return, freeing
   a state ID) or reclaims it, with the operations that carry it, which steps
   1, 2 and 4 still gate; step 3's refusal and the ceiling's do not apply to a
   call that only releases state
   ([RFC 19 §3.1](rfc-19-authorization.md#3.1%20One%20request%2C%20in%20order)).

In a cluster the gateway's router reads the handle's shard to route it, but
nothing it learns reaches the client: a handle it cannot place goes to the
local filesystem, which admits first ([§2.1](#2.1%20What%20runs%20where)).

Then the shard's `Authorize` applies the share access as a ceiling and
evaluates the file (§4.6). **There is no call that skips admission**: lock,
unlock, test-lock, watch, grant acknowledgement and the calls that name only an
open are admitted like a read; NFSv3 `MNT` and the NLM calls meet step 2 as
[RFC 21 §6.3](rfc-21-nfs.md#6.3%20MOUNT) and [§6.1](rfc-21-nfs.md#6.1%20NLM)
decide. Admission is a property of the share a call
reaches, not of how it reached it, so an NFSv4 compound that `PUTFH`s a handle
and one that walks the pseudo-filesystem into the share meet the same five
checks. A compound crossing from one share into another is re-admitted, and
re-resolved, at the crossing. The result of steps 1–4 may be cached per
(share, identity) only with `ExportPolicy.Version`, the netgroups' versions,
`ShareList.Version`, the share's grant-list version
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)'s
`S‖id‖gv`) and the principals generation in the key, beside everything RFC 18 §7.1 lists; a cache of a result step 3 decided — a client rule or a netgroup match — also keys on the `Identity`'s `Client` address, since `Identity.Key()` leaves it out ([RFC 18 §6.1](rfc-18-identity.md#6.1%20From%20Identity%20to%20caller)) and two clients of one principal can be ruled differently: the principals generation is what
drops a cached resolution when a principal, a group or a mapping changes
([RFC 18 §7.2](rfc-18-identity.md#7.2%20Local%20changes%20take%20effect%20within%20a%20second)),
so a disabled user's next call resolves afresh. Step 5 reads the pools as they
are and is never cached.

![Admission on every call: a PUTFH of a held handle, a walk of the pseudo-filesystem from PUTROOTFH, and a call naming only an open all reach the share of the handle or open they name and meet the same five steps — share state (ErrDelay while quiesced, ErrStale once being removed or retired), the MinProof and MinProtection minimums (ErrWrongSecurity), client rules (ErrAccess or a narrowing to read-only), resolution (map, squash, grants; refuses nothing) and the share cap on request buffers (ErrDelay at once, except for release or reclaim) — then Authorize; a compound crossing shares is re-admitted and re-resolved, and a cached result of steps 1–4 is keyed with the policy, netgroup, share-list and grant-list versions and the principals generation, plus the client address when a client rule decided](img/rfc17-admission.svg)

**A share's removal joins the calls admitted before it.** Admission counts
the call against its share *before* it reads the share's state, and uncounts it
when the call is answered or refused; the count is kept by the filesystem that
admits, the file's primary. Once the share's removal has reached step 3, the
node calls `Filesystem.JoinShare`, which waits for the count to reach zero, and only then
RFC 14's `DropShare`, which ends the share's open state: that is step 3 of
[RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share), which
the node runs for every shard it is primary of whose `rm` row is not yet
offloaded, while serving and again at `Node.Start` (§2.5), so a primary that
crashes partway finds the shard again however far the coordinator has moved
the share's `st`, alongside the engine's own join
([RFC 8 §2.3](rfc-8-engine.md#2.3%20One%20engine%20per%20node%3B%20a%20share%20is%20a%20storage%20binding)).
Counting before checking closes the window: a call that read the state before
the mark had counted itself first, so the join waits for it, and one that
counts after the mark reads the mark and is refused. A create, link or rename
enters the engine only to draw its versions, which counts nothing, so only this
count holds one admitted before the mark from committing a record after the
removal has passed it.

**The service owns every step of reaching a share; the adapter only
translates.** What the protocols share — resolving a name or path, admission,
the pseudo-filesystem, what `SECINFO` answers and which shares a client may
list — is written once, in the filesystem. `Root` resolves a share by the path or
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
exists in the metadata store: SMB sessions, tree connects and their IDs live in the
memory of the node holding the connection, and a client that reconnects sets them up again before
it reclaims durable or persistent opens, a reclaim refused unless the
reclaiming tree connect's `ShareID` is the open's
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens));
`ClientID` is the only client state that is stored, and it is RFC 14's.

> decision: the NFSv3 MOUNT table is not kept: `DUMP` returns an empty list,
> `UMNT` and `UMNTALL` change nothing, and `showmount -a` lists nothing, which
> every client tolerates. It is advisory in the protocol; keep one only if an
> operator workflow is shown to need it, and then per node and labelled as such.

**The NFSv4 pseudo-filesystem is a function of the share list.** The filesystem on
every node builds it from the share path index, and must build the
same one, because a client moved between nodes presents the handles it already
holds. A pseudo directory lists only the components that lead to a share the
caller would be admitted to. Its handles, numeric ids, `fsid`, change attribute
and mount crossings are [RFC 21 §7.4](rfc-21-nfs.md#7.4%20Pseudo-filesystem%2C%20SECINFO%20and%20locations)'s;
the service computes them and returns them in the `File` it answers with.

**Share enumeration** — SMB `NetShareEnumAll` and `NetShareGetInfo`, NFS
`showmount -e` — is `ListShares`: the shares whose policy is not hidden and to
which the caller would be admitted; `NetShareGetInfo` is that list filtered by
name.

## 5. Orchestration

The filesystem is the only code that orders a client operation across components.
Each order runs in the filesystem at the file's primary, where the gateway has
routed the call in a cluster (§2.1). Each order below is normative; the pseudocode beside it is indicative, and cites
the RFC that owns each step rather than restating it.

### 5.0 One lock order

**One lock order, for every call.** A call takes what it needs in this order,
and never the reverse: first the admission lock of every file it touches — the
open-state step's outer level, which `lockorder` keys by file ID whether or not
the file has open state, and under which the call makes every mutating
open-state call and every check on that file (§4.8,
[RFC 14 §3](rfc-14-open-state.md#3.%20One%20table%20per%20file%2C%20at%20one%20primary))
— in file-ID order; then fold
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

**The open-state step has two levels.** The admission lock above is the
service's, held across a compound operation — a check and the I/O or change it
admits. The table's own entry mutex is a leaf inside one table call or one
timer step, and is never held when a call returns. Timer-driven table changes
take the entry mutex alone, so one can land between a `CheckIO` and the write it
admits; each only removes or relaxes state, which no admitted write depends on.
Wherever the table may grant or revoke on a file the call does not name —
serving a file's waiters in the timer's steps, and after `AckRecall`,
`FreeState`, `Clients.Connect`, `Expire`, `Revoke`, `Disconnect` and
`Locks.ReleaseLockOwner` — it takes that file's admission lock itself, through
the `order` function the node hands the table at `Start`
([RFC 14 §11.2](rfc-14-open-state.md#11.2%20Lifecycle)), so the service calls
those holding no admission lock. Every table
call in the sketches below passes `c.Who()`: the `Call`'s client and
protocol and the mapped principal, as RFC 14's `Caller`, and names a file by `keyOf`: its
share, shard and ID, as RFC 14's `FileKey`.

**A held call holds nothing.** A call the service holds behind a clone or a copy's
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
| 1 | admit the call to the share its handle names, counted first; resolve the handle, refusing a stale one | §4.9 |
| 2 | through an open: `NotYours` for a caller that is not its principal, then the open's stored grant; through a delegation: `NotYours` for a client that does not hold it; anonymous or delegated: authorise the caller against the file and check deny modes others hold, breaking handle caching before refusing | §3.1, [§4.6](#4.6%20One%20chokepoint), §5.3 |
| 3 | in grace, `ErrGrace` for a write overlapping a lock, deny mode or grant that may still be reclaimed; otherwise the caller's open and others' byte-range locks; a conflicting caching grant starts a recall and returns `ErrDelay` | [RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard), [§7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols) |
| 4 | reserve the charge the write adds, if it extends the file | §5.6 |
| 5 | `ErrPerm` for an immutable file, or an append-only one below its size, releasing step 4's reservation, before anything changes; then, for a caller not privileged — the owner included — on a setuid file, or setgid with group execute, clear those bits in a committed mode change. The same step runs for a truncate, a deallocate and a copy or clone's destination | [RFC 7 §7.8](rfc-7-namespace-metadata.md#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint), [§9.6](rfc-7-namespace-metadata.md#9.6%20setuid%20and%20setgid%20are%20cleared%20when%20an%20unprivileged%20caller%20changes%20the%20file) |
| 6 | under the file's accept lock: check step 5's flags again against the overlay's size; capture `WCC.Before`; the engine's `Write`, passing the times the open has suspended; `WCC.After`; release the lock; for a stable write, the engine's `Commit` over the extent. Every other change of a file draws its version from `Draw` under the same lock; a change refused `ErrVersionBehind` is redrawn and retried within the deadline, never surfaced, and `DelayConflict` at it | [RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more), [RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps), [§9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward), [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open) |
| 7 | reply: a stable write once the journal has synced it **and**, if it overwrites committed content, its existence commit has landed; a stable append or hole fill waits only for the sync. The group commit records existence, usage and `mtime`, and releases the reservation | [RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal) |

Steps 3–6 run at the primary as one step (§4.8), except across a hold (§5.0).
The lock never spans a sync or a group commit.

```go
func (s *Filesystem) Write(ctx context.Context, call Call, ref OpenRef, off int64, src []byte, st Stability) (WriteResult, error) {
	c, done, err := s.admit.Call(ctx, call, ref.Handle) // step 1: counted, then admitted (§4.9)
	if err != nil {
		return WriteResult{}, err
	}
	defer done()
	f, err := s.resolve(ctx, ref.Handle) // ErrStale (RFC 7 §6.3)
	if err != nil {
		return WriteResult{}, err
	}
	for {
		// §5.9: wait behind a running clone or copy chunk holding nothing (§5.0),
		// at most WithHold's max, then ErrDelay(DelayClone).
		if err := s.waitClone(ctx, f, span(off, src)); err != nil {
			return WriteResult{}, err
		}
		wr, retry, err := s.writeOnce(ctx, c, ref, f, off, src, st)
		if !retry {
			return wr, err
		}
	}
}

// writeOnce runs steps 2–7 once. retry is set, with everything it took
// released, when a clone began on the extent after waitClone returned.
func (s *Filesystem) writeOnce(ctx context.Context, c Caller, ref OpenRef, f File, off int64, src []byte, st Stability) (wr WriteResult, retry bool, err error) {
	unlock := s.order.OpenState(f.ID) // §5.0
	defer unlock()
	if err = s.authz.IO(ctx, c, ref, f, Write); err != nil { // step 2: §4.6
		return wr, false, err
	}
	if err = s.checks.CheckIO(ctx, c.Who(), keyOf(f), ref, span(off, src), true); err != nil { // step 3: ErrGrace, ErrLocked, ErrShareViolation, ErrDelay(DelayRecall)
		return wr, false, err
	}
	res, err := s.quota.Reserve(ctx, c, ownerOf(f), f, growth(f, off, src)) // step 4: §5.6
	if err != nil {
		return wr, false, err
	}
	if err = s.flagsAndSetuid(ctx, c, f, off); err != nil { // step 5: ErrPerm; setuid cleared, committed
		res.Release()
		return wr, false, err
	}
	s.order.Accept(f.ID, func() { // step 6, under the accept lock
		if s.clones.Overlaps(f, span(off, src)) { // a clone began since waitClone: hold nothing (§5.0)
			retry = true
			return
		}
		wr.WCC.Before = s.captureBefore(ctx, f) // nil where it cannot be taken (§5.7)
		// The reservation travels with the staged bytes: the existence commit that
		// records their charge releases it (step 7), never this function's return.
		wr.Verifier, err = s.engine.Write(ctx, f, off, src, opts(ref, st, res))
		wr.WCC.After = s.overlaid(ctx, f) // same hold, once staged
	})
	if retry || err != nil {
		res.Release()
		return WriteResult{}, retry, err
	}
	if stable(ref, st) {
		_, _, err = s.engine.Commit(ctx, f, off, int64(len(src))) // step 7: RFC 8 §5.1
	}
	wr.N = len(src)
	return wr, false, err
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
capacity — returns `ErrDelay` (`DelayCapacity`) while offload or repack can
free space, never `ErrNoSpace` for a deadline
([§4.3](#4.3%20Errors%20are%20neutral%20values)); a logical quota refuses at
once (§5.6).

### 5.2 Read

Steps 1–3 as for a write, with read access; then the engine's `Read`, which
resolves residency and streams verified chunks
([RFC 8 §7.2](rfc-8-engine.md#7.2%20The%20reply%20holds%20only%20verified%20chunks%2C%20in%20file%20order)).
It never returns an unverified byte, but it can return `n` verified bytes and
then an error (`ErrLost`, `ErrUnavailable`, `ErrCorrupt`) for the chunk after
them. An adapter answers a short read of the `n` bytes when `n > 0` and its
protocol allows, and the error otherwise; `ErrUnavailable` is transient
(held, as §3.2 says). The wire statuses are
[RFC 21 §11.5](rfc-21-nfs.md#11.5%20Errors)'s
and [RFC 22 §16.5](rfc-22-smb.md#16.5%20Errors)'s. A
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
func (s *Filesystem) openAt(ctx context.Context, c Caller, r OpenRequest) (OpenResult, error) {
	if r.Reconnect != nil { // SMB DHnC, DH2C: RFC 14 §8.1, matched on client, principal, share, create GUID, lease key
		return s.reconnect(ctx, c, r)
	}
	for {
		seen, _ := s.lookup(ctx, c, r.Parent, r.Name)          // unlocked: learns which steps to take
		unlock := s.order.OpenState(fileOf(r.Parent), seen.ID) // §5.0: parent and file, in file-ID order
		res, again, err := s.openLocked(ctx, c, r, seen)
		unlock()
		if !again {
			return res, err
		}
	}
}

// openLocked runs holding the parent's and the named file's open-state steps;
// again is set when the name no longer names seen, and the steps are re-taken.
func (s *Filesystem) openLocked(ctx context.Context, c Caller, r OpenRequest, seen File) (OpenResult, bool, error) {
	f, exists := s.lookup(ctx, c, r.Parent, r.Name)
	if f.ID != seen.ID {
		return OpenResult{}, true, nil
	}
	var dir *WCC
	if !exists { // disposition create, open-if, overwrite-if, supersede
		if !r.MayCreate() {
			return OpenResult{}, false, ErrNotFound
		}
		// §5.10's create, in its order: add-file on the parent, then its delete
		// pending (§5.4), then one inode charged to the new file's owner (§5.6).
		// The new file's own step is taken last: no other call can name it yet.
		cr, err := s.createLocked(ctx, c, r.Parent, r.Name, r.Create)
		if err != nil {
			return OpenResult{}, false, err
		}
		f, dir = cr.File, &cr.Dir
	}
	if err := s.authz.Check(ctx, c, f.Handle, r.Parent, r.Access); err != nil { // §4.6: via the parent, so DELETE comes from delete-child
		return OpenResult{}, false, err
	}
	req := toOpenState(r, f)                                       // Name, Stream, Owner, Via, Seqs and the rest of RFC 14's OpenRequest
	req.Loss = s.engine.LossSeq(f.ID)                              // RFC 14 §8.1: what a flush through this open compares with
	o, err := s.opens.Open(ctx, c.Who(), keyOf(f), req)            // keyOf: share, shard and file. RFC 14 §6: deny modes; a handle-caching grant over a
	if err != nil {                                                  //   conflict is broken first, ErrDelay(DelayRecall): no worker waits (§3.2)
		return OpenResult{}, false, err
	}
	max, _ := s.authz.Granted(ctx, c, f.Handle, r.Parent) // MaximalAccess, without a backup intent's privilege
	return OpenResult{Ref: refOf(o.Open, f.Handle), File: f, MaximalAccess: max, Grant: o.Grant, Confirm: o.Confirm, Created: dir != nil, Dir: dir}, false, nil
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
`Opens.Update` ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)).
When the file's last open closes, `Close` runs the deferred unlink of the
recorded entry once it has released the file's open-state step: as §5.4's
unlink, taking the parent's and the file's steps afresh in file-ID order, and,
when the parent's primary is another node, as RFC 15 §4.2's coordinator
(§4.8). Holding the file's step while it took the parent's would deadlock with
an unlink of another of the file's names, which takes them in the other order.
It runs as the principal that marked the file and through the marking open's
`OpenGrant`, so that open's grant — a backup or restore intent's privilege
included — authorises it ([§4.6](#4.6%20One%20chokepoint),
[RFC 7 §8.2](rfc-7-namespace-metadata.md#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later)),
and returns the parent's `WCC`. One still refused is logged at `Warn`, counted
(§7), and the file keeps its name: the client has closed, and no reply reaches
it.

**The service runs every release.** It calls the engine's `Delete`
([RFC 8 §8.1](rfc-8-engine.md#8.1%20A%20removal%20is%20one%20transaction%2C%20then%20batches))
at the file's primary, at four moments: when `Unlink` or `Rename` reports a file
left with no entry and no open holds it; when open state's `Close` reports the
last close of a file with no entry, having deleted its durable open record
([RFC 14 §9.1](rfc-14-open-state.md#9.1%20An%20open%20keeps%20a%20file%20alive)),
or reports through `Sink.Closed` a close the table made itself — a timeout,
grace's end, an expiry or revoke ([RFC 14 §11.1](rfc-14-open-state.md#11.1%20Package%2C%20dependencies%20and%20wiring));
when a release commits that wrote the pending releases of its file's named
streams ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)),
for each of those streams; and in recovery, for every pending release, when the
table raises `Sink.GraceEnded(share, shard)` (§2.5), read with `PendingReleases(share, shard)`, never by a
scan of the share's files. Before each `Delete` the service takes the file's
admission lock — at a last close, a `Sink.Closed` and `GraceEnded` it holds
none yet — and marks the file through `Opens.Releasing`, which fails if an open
exists; it reports the release's outcome to the `done` that returns, so an
aborted release clears the mark
([RFC 14 §9.1](rfc-14-open-state.md#9.1%20An%20open%20keeps%20a%20file%20alive)).
The release transaction re-checks its holders itself
([RFC 7 §4.5](rfc-7-namespace-metadata.md#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction)),
so a release begun on a stale view aborts and loses nothing.

### 5.4 Remove and rename

The metadata store applies the namespace change. The final unlink writes the
file's pending-release record in the same transaction, and holders are only the
durable open records; the release transaction deletes the record once no open
remains ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)),
and the service runs it (§5.3). A crash between the unlink and the release leaks
nothing. A rename that replaces a target applies the same rule to the target.

```go
// unlinkAt serves Unlink and Rmdir (rmdir set).
func (s *Filesystem) unlinkAt(ctx context.Context, c Caller, dir Target, name []byte, rmdir bool) (UnlinkResult, error) {
	for {
		seen, err := s.lookup(ctx, c, dir.H, name) // unlocked: learns which steps to take; ErrNotFound
		if err != nil {
			return UnlinkResult{}, err
		}
		unlock := s.order.OpenState(fileOf(dir.H), seen.ID) // §5.0: both files' steps, in file-ID order
		r, again, err := s.unlinkLocked(ctx, c, dir, name, seen, rmdir)
		unlock()
		if !again {
			return r, err
		}
	}
}

func (s *Filesystem) unlinkLocked(ctx context.Context, c Caller, dir Target, name []byte, seen File, rmdir bool) (UnlinkResult, bool, error) {
	f, err := s.lookup(ctx, c, dir.H, name)
	if err != nil || f.ID != seen.ID { // the name moved between the lookup and the steps
		return UnlinkResult{}, err == nil, err
	}
	if rmdir && f.Type != Dir {
		return UnlinkResult{}, false, ErrNotDir // Rmdir
	}
	// Delete-child on the parent, or delete on the file; the sticky rule; immutable parent.
	// Through an open, its grant decides (§4.6, RFC 7 §7.7).
	if err := s.authz.Name(ctx, c, dir, f, Delete); err != nil {
		return UnlinkResult{}, false, err
	}
	// Opens of others: a deny-delete refuses after handle caching is broken (§5.3);
	// a grant on the file is recalled (ErrDelay); an SMB delete of a directory with
	// an open child is ErrShareViolation (RFC 14 §9.5). dir.Open, when set, is an open
	// of f, the one the delete came through (§3.0's Target), never the directory's.
	if err := s.checks.CheckChange(ctx, c.Who(), keyOf(f), dir.Open, ChangeRemove); err != nil {
		return UnlinkResult{}, false, err
	}
	var r UnlinkResult
	var orphan Orphaned
	s.order.Accept(func(v Versions) { // the directory's and the file's accept locks; draws both versions (RFC 7 §9.4)
		r.Dir.Before = s.captureBefore(ctx, dir.H) // nil for a directory over NFSv3 (§5.7)
		// One transaction: the entry, Nlink − 1, usage, and for the last link the
		// pending release (RFC 7 §4.1, §4.3); ErrNotEmpty for a directory with entries.
		orphan, err = s.meta.Namespace.Unlink(ctx, c.Resolved(), v, dir.H, name, grantOf(dir.Open))
		r.Dir.After = s.overlaid(ctx, dir.H)
	}, fileOf(dir.H), f.ID)
	if err == nil && orphan.File != 0 {
		err = s.engine.Delete(ctx, orphan.File) // aborts while an open holds it (RFC 7 §4.5); the last close releases (§5.3)
	}
	return r, false, err // the chunks it referenced are dropped by the engine's batches and GC (RFC 8 §8.1)
}
```

`Rename` takes the same steps for the source entry and, when the target name
exists, for the target:

```go
func (s *Filesystem) renameAt(ctx context.Context, c Caller, from Target, fromName []byte, to Handle, toName []byte, noReplace bool) (RenameResult, error) {
	src, err := s.lookup(ctx, c, from.H, fromName) // ErrNotFound
	if err != nil {
		return RenameResult{}, err
	}
	dst, exists := s.lookupIf(ctx, c, to, toName)
	unlock := s.order.OpenState(fileOf(from.H), fileOf(to), src.ID, dst.ID) // §5.0; re-checked below as unlink does
	defer unlock()
	switch {
	case exists && noReplace:
		return RenameResult{}, ErrExist
	case exists && dst.Type == Dir && src.Type != Dir, exists && dst.Type != Dir && src.Type == Dir:
		return RenameResult{}, typeMismatch(dst) // RFC 7 §5.4: ErrIsDir or ErrNotDir
	case exists && dst.Type == Dir && !empty(dst):
		return RenameResult{}, ErrNotEmpty
	}
	if err := s.authz.Rename(ctx, c, from, src, to, dst); err != nil { // delete-child at from, add-entry at to; through an open its grant
		return RenameResult{}, err
	}
	if err := s.checks.CheckChange(ctx, c.Who(), keyOf(to), nil, ChangeAddEntry); err != nil { // ErrDeletePending: the target directory is marked (§5.4)
		return RenameResult{}, err
	}
	// A rename of a delete-pending file, a deny-delete on src or a replaced dst, and an
	// SMB rename of a directory with an open child are refused; grants are recalled
	// (RFC 14 §7, §9.4, §9.5). An SMB rename over a target open by anyone is refused (RFC 22 §13).
	if err := s.checks.CheckChange(ctx, c.Who(), keyOf(src), from.Open, ChangeRename); err != nil {
		return RenameResult{}, err
	}
	if exists {
		if err := s.checks.CheckChange(ctx, c.Who(), keyOf(dst), nil, ChangeRemove); err != nil {
			return RenameResult{}, err
		}
	}
	var r RenameResult
	var orphan Orphaned
	s.order.Accept(func(v Versions) { // both directories, the moved file and a replaced target: RFC 7 §11.1 refuses a change to a File missing from v
		r.From.Before, r.To.Before = s.captureBefore(ctx, from.H), s.captureBefore(ctx, to)
		// One transaction; the loop check — a directory into its own subtree, ErrInvalid —
		// is inside it (RFC 7 §5.2); ErrCross(CrossProject) across tree quotas (RFC 7 §2.8).
		// Across two primaries this filesystem, as coordinator, prepares at the other (RFC 15 §4.2).
		orphan, err = s.meta.Namespace.Rename(ctx, c.Resolved(), v, from.H, fromName, to, toName, grantOf(from.Open))
		r.From.After, r.To.After = s.overlaid(ctx, from.H), s.overlaid(ctx, to)
		if err == nil && src.Type == Dir && !sameDir(from.H, to) { // its ".." changed
			moved := WCC{After: s.overlaid(ctx, src.Handle)}
			r.Moved = &moved
		}
	}, fileOf(from.H), fileOf(to), src.ID, dst.ID)
	if err == nil { // still under the admission locks: the opens made through the old entry follow it (RFC 14 §9.4)
		if rerr := s.opens.Renamed(ctx, keyOf(src), fileOf(from.H), fromName, fileOf(to), toName); rerr != nil {
			s.log.Warn("open names not moved", "err", rerr) // the rename committed; a stale record is safe (RFC 14 §9.4)
		}
	}
	if err == nil && orphan.File != 0 {
		err = s.engine.Delete(ctx, orphan.File) // the replaced target; aborts while an open holds it (RFC 7 §4.5)
	}
	return r, err
}
```

**A delete-pending directory takes no new entries.** Before it applies a create
— an `Open` that creates included —, a link or a rename, the service **MUST** check the open state of the destination
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

The service is the one enforcer of quotas. Neither the metadata store nor the
engine refuses a write for quota.

```go
// Reserve, at step 4 of a write, a create (one inode) or a copy's destination.
// owner is whom the user and group scopes charge: the file's owner, or for a
// create the owner the new file will have — the resolved caller after
// squashing (RFC 7 §7.4) — never the parent directory's.
func (q *Quota) Reserve(ctx context.Context, c Caller, owner Owner, f File, bytes int64) (*Reservation, error) {
	for _, s := range []Scope{QuotaShare, QuotaProject, QuotaUser, QuotaGroup} { // §4.3's order
		lim, ok := q.limit(ctx, s, owner, f) // a principal's own quota, else the share's default for its kind
		if !ok {
			continue
		}
		used := q.committed(ctx, s, owner, f) + q.unfolded(ctx, s, owner, f) // committed totals plus unfolded deltas (RFC 16 §4.4)
		if used+q.reserved(s, owner, f)+bytes > lim.Hard(q.clock) {         // soft limit as hard once its grace has run
			q.events.Quota(ctx, QuotaEvent{ /* hard refusal */ })
			return nil, &QuotaError{Scope: s}
		}
	}
	return q.add(owner, f, bytes), nil // released by the write's Done, called exactly once (RFC 8 §5.1), or on failure
}
```

- A reservation is volatile, held in the primary's memory: after a crash the
  journal re-applies the writes and their charge before the file is served, and
  nothing is reserved again ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)). A write the engine accepted
  releases its reservation exactly once, through `Done`, whether its existence
  commits, is committed from a loss record, or is dropped uncommitted — with a
  failed window, a journal device lost whole, or a shard this node stops
  serving.
- **Overshoot bound.** On one primary every reservation is seen, so a hard
  limit is exceeded by at most what transactions committing concurrently add.
- **(cluster) Slack.** A primary sees its own reservations, not other
  primaries'. Where a quota's writers span primaries, each primary also refuses
  with `ErrDelay` (`DelayCommit`) a reservation that would take its
  uncommitted share past the slack `min(S, f × Hard)`, so writers through `k`
  primaries exceed a hard limit by at most `(k − 1)` times the slack plus what
  commits concurrently add. `S` and `f` are settings of
  [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings);
  the derivation is [RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)'s.

> ponytail: no slack while every primary of a share is one node, where it
> bounds nothing and only delays writes. Add it with the cluster's remote
> primary, as the bullet above states it.
- **Soft limits.** Crossing the advisory threshold or the soft limit emits a
  quota event (§3.3) and refuses nothing. A grace that expires, a hard refusal
  and a fall back under a limit each emit one too; whether they refuse is the
  hard limit's rule above and the enforcement below. The metadata store records
  when the soft limit was first exceeded, and once the grace time has run from
  then the soft limit is enforced as the hard one. Falling back under it clears
  the record. The time is the store's `Now`, so the grace time ends up to the
  clock-offset bound σ early or late; a quota's grace is not a fence and
  tolerates it.

### 5.7 GetAttr

The metadata store is a leaf: it never calls the engine. `GetAttr` joins
`Files.Get` with the engine's `Overlay` — `Size`, `Times` and `Version` of the
operations staged but not yet committed, replayed in version order — at the
file's primary, where both are local
([RFC 8 §12.1](rfc-8-engine.md#12.1%20One%20content%20interface%2C%20called%20by%20the%20filesystem%20service)). With nothing pending the File record
answers alone. Otherwise the size is `Overlay.Size` when `SizeSet` — a
truncate is pending, so it is exact — and the larger of it and the File
record's size when not; the times and `Version` are the overlay's. The two are read under the file's commit
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
between the capture and the operation — and never for a directory over NFSv3
([RFC 21 §12](rfc-21-nfs.md#12.%20Invariants)'s N19); where the lock cannot be
taken it is nil: for a write that waited on a recall, a contended lock, or
**(cluster)** a call whose two files sit at two primaries. `After` is read in the same hold once
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
  (`OtherShard`), changing nothing, so the client copies the bytes itself.
- **`Copy` is not atomic.** The engine's `Copy` runs bounded chunks, each a
  small atomic clone, and returns the bytes copied; at the caller's deadline it
  stops after its current chunk and answers a short count, from which the
  client continues. A copychunk's chunks are one call's `Extents`, run in
  order, and the count is the bytes copied across them, so the adapter derives
  chunks and bytes written from it. A chunk refused after others are done
  answers the same short count; only a refusal of the first chunk returns its error. Between
  shards the destination's filesystem copies the bytes itself, reading at the
  source's primary in bounded chunks, so no adapter composes a copy
  (§4.1). A copy is always synchronous, as copychunk and NFSv4.2 `COPY` are
  served today; RFC 14's asynchronous copy state
  ([RFC 14 §2.7](rfc-14-open-state.md#2.7%20Copy)) is unused until a client is
  shown to need offload (§9).

1. admit both handles, and authorise read on the source and write on
   the destination, as §5.1 steps 1–3 do for each;
2. refuse with `ErrInvalid` a clone of other than one extent, and a copy or
   clone any of whose extents, with source and destination one file, overlap, as Linux and RFC 7862 refuse it; with
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
held at the primary until that clone or chunk ends and then runs (§5.0), so each
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

**Lookup.** The directory's filesystem admits the call and reads the entry; the
entry's primary joins the overlay. A directory walk is the client's: the service resolves one name per
call and never follows a symlink — the client does
([RFC 22 §12.4](rfc-22-smb.md#12.4%20Reparse%20points)).

```go
func (s *Filesystem) Lookup(ctx context.Context, call Call, dir Handle, name []byte) (Handle, File, error) {
	if s.pseudo.Is(dir) {
		return s.pseudo.Lookup(ctx, call, dir, name) // a pseudo directory, or the junction into a share
	}
	c, done, err := s.admit.Call(ctx, call, dir) // §4.9; a Lookup names no open, so call.Client goes unused
	if err != nil {
		return nil, File{}, err
	}
	defer done()
	switch string(name) {
	case ".":
		file, err := s.getattr(ctx, c, dir)
		return dir, file, err
	case "..":
		if s.isShareRoot(dir) { // ".." at a share root answers the root itself; NFSv4 LOOKUPP leaves to the pseudo-fs
			return s.parentOfRoot(ctx, c, dir)
		}
	}
	// Traverse (execute) on dir is checked inside RFC 7's Lookup, through the chokepoint (RFC 7 §7.1).
	h, file, err := s.meta.Namespace.Lookup(ctx, c.Resolved(), dir, name) // ErrNotFound; a name compared by the share's case rule (RFC 7 §3.3)
	if err != nil {
		return nil, File{}, err
	}
	if share(h) != share(dir) { // a junction into another share: admitted and resolved again there
		c2, done2, err := s.admit.Call(ctx, call, h)
		if err != nil {
			return nil, File{}, err
		}
		defer done2()
		c = c2 // the overlay, and anything after, acts as the caller the new share resolves
	}
	file, err = s.overlay(ctx, c, h, file) // the file's primary: §5.7
	return h, file, err
}
```

**Create.** At the parent's primary; `Open` with a create disposition runs the
same steps under the steps it holds:

```go
func (s *Filesystem) createAt(ctx context.Context, c Caller, dir Handle, name []byte, a CreateArgs) (CreateResult, error) {
	unlock := s.order.OpenState(fileOf(dir))
	defer unlock()
	return s.createLocked(ctx, c, dir, name, a)
}

// createLocked runs holding the parent's open-state step; Open calls it too (§5.3).
// A new file placed in another shard is created by RFC 15 §4.2's two-primary
// path, with this filesystem as coordinator.
func (s *Filesystem) createLocked(ctx context.Context, c Caller, dir Handle, name []byte, a CreateArgs) (CreateResult, error) {
	// add-file or add-subdirectory on the parent (RFC 7 §7.7)
	if err := s.authz.Check(ctx, c, dir, Handle{}, addRight(a.Kind)); err != nil {
		return CreateResult{}, err
	}
	if err := s.checks.CheckChange(ctx, c.Who(), keyOf(dir), nil, ChangeAddEntry); err != nil {
		return CreateResult{}, err // ErrDeletePending (§5.4)
	}
	res, err := s.quota.Reserve(ctx, c, ownerOf(c, a), File{}, inode) // one inode, charged to the new file's owner (§5.6)
	if err != nil {
		return CreateResult{}, err
	}
	var r CreateResult
	s.order.Accept(func(v Versions) { // the parent's accept lock; v holds its version and the new file's
		r.Dir.Before = s.captureBefore(ctx, dir)
		// One transaction: the name validated (RFC 7 §3.2, §3.8: ErrNameTooLong,
		// ErrInvalid); owner and group from the resolved caller after squashing; the
		// inherited ACL; a case-fold collision is ErrExist under the share's case rule
		// (RFC 7 §3.3); an exclusive create stores the verifier, and a retry whose
		// verifier matches returns the file, any other ErrExist (RFC 7 §2.10).
		r.Handle, r.File, err = s.meta.Namespace.Create(ctx, c.Resolved(), v, dir, name, attrsOf(c, a))
		r.Dir.After = s.overlaid(ctx, dir)
	}, fileOf(dir), NewFile)
	if err != nil {
		res.Release()
	} else {
		res.Commit(r.File) // the charge now rides on the create's own commit
	}
	return r, err
}
```

### 5.11 Locks

```go
func (s *Filesystem) lockAt(ctx context.Context, c Caller, ref OpenRef, r LockReq) (StateRef, error) {
	if err := s.authz.LockState(ctx, c, ref); err != nil {
		return StateRef{}, err
	}
	// RFC 14 §11.3: every range in one step, all or none, and only a single range
	// may wait. Grace (ErrGrace unless r.Reclaim), conflicts with every protocol's
	// locks, a recall of a conflicting grant (ErrDelay, or ErrBlocked with a waiter
	// when waiting), the stateid's seqid (StateError OldState) and the NFSv4.0
	// owner's sequence (StateError BadSeqID), checked and advanced at the owner's
	// home primary as RFC 14 §2.8 says: this node on a single node; (cluster)
	// asked through Peers.Sequence first (§2.3).
	res, err := s.locks.Lock(ctx, c.Who(), keyOf(ref.Handle), toOpenState(ref, r)) // a conflict is *LockedError, naming the holder
	if err == nil && res.Waiting {
		return StateRef{}, ErrBlocked // a waiter is registered; LockWaitOver answers it
	}
	return res.Ref, err // the lock state's reference with its new Seq: NFSv4 LOCK's stateid
}
```

A waiting lock holds no worker and has no deadline; it ends only as RFC 14's
waiter does ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)).

## 6. Invariants, and the test of each

Every test runs in §8's integration tier unless it names another tier. A row is here only if its test can fail against a plausible wrong service; what a type, the compiler or another RFC's suite already checks is not repeated (adapter imports: [RFC 20 §7.1](rfc-20-adapters.md#7.1%20Imports%20and%20call%20sites); held requests: [RFC 20 §7.4](rfc-20-adapters.md#7.4%20Cross-protocol%2C%20through%20both%20adapters); revoked and stale state: [RFC 14](rfc-14-open-state.md)). Numbers of removed rows are not reused.

| # | Invariant | Test |
| --- | --- | --- |
| V3 | Every cross-component order is the one in §5, in every protocol. | Each order is tested by what it prevents, through each protocol's `Call`: a write refused at step 3 holds no reservation, stages no byte and clears no setuid bit; one refused at step 5 holds no reservation; a create refused add-file charges no inode and adds no entry, and is refused `ErrAccess` before a delete-pending parent could answer `ErrDeletePending`. |
| V4 | A routing refusal never reaches an adapter. | **(cluster)** Move a shard's primary between a call's routing and its arrival; assert the gateway re-routes and the call succeeds. |
| V6 | A recall not acknowledged by its deadline revokes the grant; no worker waits on a recall. | A conflicting operation returns `ErrDelay` while its recall is pending, with no goroutine blocked; a recall that misses its deadline delivers `RevokedGrant` naming the grant, `Renew` reports `RecallableRevoked`, a write under its stateid returns `ErrState` (`Revoked`), not `NotYours`, and the client's next open is offered no grant while the revoked one is unfreed. |
| V7 | The conflict check and the I/O it admits run at one primary, and no conflicting state is granted between them. | A model test interleaves a lock and a write: the lock is ordered wholly before or after the write. |
| V8 | Every open-state call names its `ClientID`, and an ID the client does not hold is refused. | An `OpenID`, `GrantID` or `WatchID` presented by another client returns `NotYours`. |
| V9 | The share grant is evaluated on every call. | A client removed from a share grant is refused on its next call with a handle it already holds; an SMB `MAXIMUM_ALLOWED` open whose parent grants delete-child includes `DELETE`, and after the parent's group changes so it no longer does, a cached delete-child decision is not reused. |
| V10 | Quota is enforced only by the service, and overshoot stays within §5.6's bound. | Writers on one primary never pass a hard limit by more than what commits concurrently add; **(cluster)** writers through `k` primaries overshoot by no more than the bound, counted per run, on a 1 GiB project quota as on a 1 TiB one; a write refused by a share-wide, project, user and group quota in turn returns a `*QuotaError` naming each scope, and with two exceeded the first in §4.3's order; `StatFS` inside a project tighter than its share reports the project's, and the share's once the project's quota is deleted. |
| V11 | (cluster, [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)) A forwarded mutation retried after a lost reply returns its original result and applies once. | Multi-node tier: a forwarded write whose reply the loopback transport drops, retried after another write lands on the same extent, applies once. |
| V12 | Every call that names a handle or an open is admitted — state, flavour, client rules, resolution — whatever path the handle arrived by. | For each of lock, unlock, test-lock and watch, reached by a handle presented directly, by `Root` with a path, by `Root` with a name, and by the zero `ShareRef` then a walk from the pseudo-root: a quiesced share returns `ErrDelay`, a share refusing the flavour `ErrWrongSecurity`, a client outside its rules `ErrAccess`; a compound crossing from a share squashing root into one that does not acts as root only in the second; a caller admitted and cached, then disabled, is refused on its next call. |
| V13 | No mount, tree connect or session is stored; a reconnecting client is admitted afresh. | A renamed share keeps serving held handles and established tree connects, and a new `Root` needs the new name. |
| V14 | Every node builds the same pseudo-filesystem from the same share list. | Pure unit: two filesystems built from one share list return byte-identical pseudo handles, numeric ids and change attributes, and a share added on one is seen on the other with a raised change attribute. |
| V16 | The `Version` `GetAttr` returns advances on every accepted write and never moves backward. | Client A reads `Version`, B writes and is acknowledged, A's next `GetAttr` is greater before any existence commit and not lower after it; 16 readers `GetAttr` while writes commit in a loop and none sees a `Version` lower than one it saw; a `chmod` drawn below a write committed or staged at a higher version is redrawn above it. |
| V17 | Every release is run by the service at the file's primary, at the four moments of §5.3. | An unlinked file whose last open closes is released, and so, after a restart, is a file whose pending release was written before the crash. |
| V18 | An open's grant authorises a call only for the principal it was granted to. | bob presents alice's open to write a file alice may write and bob may not, then one both may write: `NotYours` both times and alice's open intact. A service that trusts the open's grant for any presenter accepts the first; one that re-authorises bob afresh accepts the second. |
| V19 | A write through a persistent open, or any open on a continuously available share, is stable; a stable write waits for the sync and, overwriting committed content, its existence commit. | A write on a persistent open, asked unstable, survives a crash injected after its reply; a stable append's reply waits for no metadata transaction, counted by the store's transaction counter; a stable overwrite stalls with the store and answers `ErrDelay` at the deadline, never success. |
| V20 | A journal-limit or capacity refusal is `ErrDelay` while space can be freed, never `ErrNoSpace` for a deadline; `ErrNoSpace` and `ErrQuota` are answered at once. | With the journal full of dirty bytes and the remote reachable, a write returns `ErrDelay(DelayCapacity)` and succeeds on retry once offload frees space; with the store unreachable past the deadline, still `ErrDelay`; with the store refusing puts as denied, `ErrNoSpace` at once. |
| V21 | A deny-mode refusal is decided only after the handle-caching grants covering the conflicting opens are broken. | An SMB open, an NFSv4 `OPEN` and an NFSv3 `WRITE` meeting a deny mode held only in an SMB handle cache each get `ErrDelay`, the lease is broken, and the retry succeeds. |
| V22 | An unprivileged caller's write, truncate, deallocate, copy or clone into a setuid or setgid file — the owner's included — clears the bits in a committed change before the bytes are staged. | bob writes alice's setuid file and `GetAttr` before any commit shows the bit clear; again with alice, unprivileged, then with a deallocate, a copy and a clone; an unprivileged append to a setuid, append-only file succeeds and clears the bit, and a write below its size is `ErrPerm` and clears nothing. |
| V23 | A blocked lock holds no worker and has no deadline; nothing answers one other than as granted before `CancelLock` returns. | NLM and SMB locks that wait are answered `ErrBlocked` and later granted through `LockWaitOver`, with no goroutine held between; an NFSv4 one is `ErrLocked` and its next poll after the unlock is granted; a lock list with one range refused leaves none of the call's ranges held. |
| V24 | I/O under a delegation is served only to the client holding it and authorised for the caller's own principal. | An NFSv4.1 client with a write delegation writes as a second principal that may write the file, then a third that may not: the first accepted, the second `ErrAccess`; another client presenting the stateid gets `NotYours`. |
| V25 | `Write` samples the verifier before staging and `Commit` after its sync; an SMB flush across a loss fails once with `ErrLost`. | A `Commit` that waits on a failed sync window returns a verifier other than its writes'; the verifier changes after an unstable write is dropped as lost in the same process and epoch, and when a shard is handed away and back, and not across a shard-epoch raise that moves nothing; an SMB flush through each of two opens of such a file fails once, then succeeds. |
| V26 | Copy and clone refuse overlap, a clone over `clone_max_len`, a namespace or (cluster) shard crossing and an `Incomplete` source, each changing nothing; a copy across shards is the service's; a Lost source run is copied as Lost; a clone is atomic, a copy may be short. | Each refusal returns its error and leaves the destination unchanged and readable; a `COPY` whose deadline passes partway answers a short count and the destination holds exactly that many bytes; a clone of a source with a Lost run reads Lost there at the destination. |
| V28 | A create — an `Open` that creates included —, link or rename into a directory marked delete on close is refused, over every protocol. | With a directory marked over SMB, a create, a creating `Open`, a link and a rename into it, over SMB and over NFS, are each refused. |
| V29 | Every call takes open-state steps, then fold admission, then accept locks, each in file-ID order; a held call holds none of them and re-runs §5.1's steps 3–6. | An open of an existing child and an unlink of it race 10⁴ times, with the child's ID below its parent's and above it: neither waits for its deadline; a write and an unlink of the file's last name race 10⁴ times with a deny-delete open: neither waits for its deadline and no deny-delete is granted between the unlink's check and its commit; a 1 GiB clone of a VM disk under writes, with a lease break pending: each held write gives up its open-state step and accept lock, the break's acknowledgement and a new open proceed during the clone, and each held write completes after re-running its checks. |
| V30 | The service waits inside a call only behind a running clone or copy chunk, and at most the call's `WithHold`. | An NFSv3 write into a cloning extent with `WithHold(10s)` is held and completes when the clone ends; with `WithHold(0)` it returns `DelayClone` at once. |
| V31 | Every mutating call returns the `WCC` of each object it changed, from one hold of its accept lock. | For each mutating method, a concurrent write between the call and a later `GetAttr` does not appear in the returned `WCC`; a directory's `Before` is nil over NFSv3. |
| V32 | The service imports no adapter and no network code; the gateway and `Serve` call no store, engine, open state or identity and hold the filesystem only as a `vfs.Service`; `Authorize` and `Granted` are called only from `authz`. | §8's architecture check, which must report each of its seeded violations. |
| V33 | `Start` is all or nothing, `New` starts nothing, and `Stop` leaves no goroutine and closes nothing it was handed. | Fail each start step in turn: `Start` returns the error and the goroutine count is back to the built state's; after `Stop`, none of the service's goroutines remain and the store, engine and open state still serve a direct call. |
| V34 | `Drain` joins every admitted call and admits none after. | With 64 writes in flight, `Drain` returns only once each has answered; a call made after `Drain` begins returns `ErrUnavailable`; a lease break pending at `Drain` is neither forced nor revoked. |
| V35 | After `Start`, a call that needs or conflicts with reclaimable state answers `ErrGrace` until the shard's grace ends, and pending releases run only after it. | Restart with a pending release and a reclaimable lock: a conflicting lock answers `ErrGrace` and the file's content is still held; advancing the fake clock past grace grants the lock and runs the release. |
| V36 | An SMB break reaches only a session holding one of the lease's opens, or one signed in as an opener, and never any other session of the client; only an opener joins or answers the lease, and a change by any other principal of the client breaks it. | mallory sends alice-pc's `ClientGuid` and signs in as herself, through this node and through a second one; build01 then writes `report.docx`, which alice-pc holds under an RWH lease on an open that shares delete: the break reaches alice's session and none of mallory's, with alice's open connected and with it disconnected, and mallory's `AckRecall` on the lease is `NotYours`. Then disconnect alice's open and end alice's session: the next break reaches no session, mallory's included, and runs to its deadline. mallory opens `report.docx` on her own connection presenting alice's lease key: the open succeeds with no lease, and she is not an opener; bob, signed in on alice-pc's connection, presenting the same key joins the lease. alice signs in again and takes a fresh, acknowledged RWH lease on an open that shares write and delete; mallory writes through her no-lease open: the break reaches alice's session and none of mallory's. alice takes another fresh, acknowledged RWH lease the same way; mallory renames another file over `report.docx`: the break reaches alice's session and none of mallory's. A `NEGOTIATE` alone, or a null or guest session, registers nothing, and a guest's open is offered no lease. |
| V37 | A create charges the new file's owner, never the parent's. | bob, at his inode quota, creates in alice's directory, which has room under alice's: `ErrQuota` (`QuotaUser`), nothing created and nothing charged to alice. |
| V38 | Delete on close takes the parent's step only after releasing the file's. | A last close running a delete on close races 10⁴ times an unlink of another name of the same file, with the parent's ID below the file's: neither waits for its deadline, and exactly one name is left. |
| V39 | Admission runs before the handle is resolved. | A client outside a share's netgroup presents a live handle and the handle of a removed file: `ErrAccess` for both. |
| V40 | A share's removal joins every call admitted before its mark. | A create is held between admission and its commit while the share is removed: step 3 waits for it; a call counted after the mark is refused `ErrStale`, its record never committed. |
| V41 | The gateway forwards and decides nothing; `Serve` fences and dedups before the filesystem runs anything. | Gateway unit, over a fake `Router` and `Transport` and `vfstest.Record` as the local `Service`: each method reaches the local `Service` when the router says local and the transport otherwise, with its arguments, `Identity` included, and its answer unchanged; a handle the router cannot place reaches the local `Service`. Multi-node tier: a call carrying a stale shard epoch, a fenced node epoch or a hop of one is refused by `Serve` and reaches no filesystem, sent over the transport or routed to the local path by a stale cache; a duplicate send applies once. |
| §3.2 | A callback path moves; notifications honour traverse; a break that times out leaves its opens usable. | A recursive watch reports nothing below a directory its holder cannot traverse; an SMB lease whose break times out leaves its open usable for the holder's next write. |
| §3.2 | A removed or moved share ends its state with `RevokedShare`. | With a waiting lock and an outstanding watch on a share, remove it, then repeat with a move naming an address: the recording `Callbacks` receive `RevokedShare` naming it, with `Addr` set for the move; the share's opens and deny modes are gone; the removal's step 3 waits for a create admitted before the mark. |
| §4.6 | A privileged handle is privileged for every request through it. | A backup-intent handle sets owner, group and DACL on a file whose DACL excludes the caller, as `robocopy /B /SECFIX` does; without backup intent each is refused; a rename through a handle opened with `DELETE` succeeds without delete-child. |
| §5.3 | A delete on close runs at the last close, through the marking open. | A backup-intent open with delete on close of a file whose DACL excludes its caller, and an EA set through it, succeed; with the parent made immutable before the close, the deferred unlink is refused, counted in `vfs_deferred_unlink_refusals_total`, and the file keeps its name. |
| §5.9 | An `Incomplete` source refuses before anything changes. | Copy and clone from a file a restored snapshot lists as incomplete into an existing 2 GiB file: `ErrIncomplete` each, the destination unchanged. |

## 7. Observability

Metric names are shown without the deployment's prefix.

| Answers | Metric | Type |
| --- | --- | --- |
| How fast is each operation, per protocol? `outcome` is `ok`, `error` or `delay` | `vfs_op_seconds{op, protocol, outcome}` | histogram |
| Which errors does each operation answer? | `vfs_errors_total{op, error}`, `error` one of §4.3's neutral errors | counter |
| How often are clients told to retry, and why? | `vfs_delayed_total{op, cause}` | counter |
| How often does quota refuse, and how much is reserved? | `vfs_quota_refusals_total{scope}`, `vfs_quota_reserved_bytes` | counter, gauge |
| How often is a call refused admission, and by which check (§4.9)? | `vfs_admission_refusals_total{check}`: `state`, `flavor`, `client_rules`, `resolution` | counter |
| How many calls are admitted and not yet answered — what a drain or a share removal waits for (§2.5, §4.9)? | `vfs_inflight_calls` | gauge |
| Are callbacks failing? | `vfs_callback_errors_total{kind}` | counter |
| How long do calls wait behind a running clone (§5.9)? | `vfs_clone_wait_seconds{outcome}` | histogram |
| How often is a delete on close refused after its client closed (§5.3)? | `vfs_deferred_unlink_refusals_total` | counter |
| **(cluster)** How many calls does the gateway forward, and how do they end? | `vfs_forwarded_total{op, outcome}`: `ok`, `rerouted`, `resent`, `failed` | counter |
| **(cluster)** What does a hop cost? | `vfs_hop_seconds{op}` | histogram |
| **(cluster)** How often does `Serve` fence a call, or answer one from the dedup table? | `vfs_fenced_total{check}`: `node_epoch`, `shard_epoch`, `hop`; `vfs_dedup_hits_total` | counter |

Operation latency, quota, forwarding, fence and dedup metrics are owned here
and nowhere else. Events dropped are counted at the sink, RFC 25's
`events_dropped_total`, not here. Held requests are the adapters'
([RFC 20 §10.1](rfc-20-adapters.md#10.1%20Metrics)); recalls are RFC 14's.

**Spans.** Each `Service` call opens one span, `vfs.<Op>`, carrying `op`,
`protocol`, `outcome`, and the `DelayCause` or `StateError` kind when it ends
in one; share and file IDs are span attributes, never metric labels
([RFC 16 §8.1](rfc-16-metadata-store.md#8.1%20Metrics)). It parents the
metadata store's and the engine's spans. Waits inside the call are span events,
not child spans: the lock order's waits with what was waited for (§5.0), a
wait behind a clone (§5.9), a redraw after `ErrVersionBehind` (§5.1).
**(cluster)** The gateway opens `vfs.forward` for each call it sends, with
`op`, the primary's node and `outcome`, and each re-route or resend as an
event. Its context travels in the call, so the filesystem's `vfs.<Op>` span at the
primary is its child, and `Serve` records a fence refusal or a dedup hit as an
event on it.

## 8. Conformance

The service is tested in tiers, cheapest first, as
[RFC 20 §7](rfc-20-adapters.md#7.%20Tests) tests an adapter. Each tier catches
something the ones above it cannot.

| Tier | What runs | Proves | Runs |
| --- | --- | --- | --- |
| unit | `admit`, `pseudofs`, `quota`, `lockorder` and `pkg/vfs`, each over a fake of the few reads it declares | admission over (share, identity) and its cache key; pseudo-filesystem determinism (V14); quota arithmetic; the lock-order sorter; the ID codecs decode RFC 21's and RFC 22's golden bytes and refuse malformed ones; `vfs.Errors()` lists every declared error | per change |
| unit | `*vfs.Filesystem` from `vfs.New`, with a fake engine (`Content`) and in-memory metadata | §5's orders by what each refusal leaves unchanged (V3) | per change |
| unit | `*vfs.Gateway` from `vfs.NewGateway`, with a fake `Router` and `Transport` and `vfstest.Record` as the local `Service` | routing, re-routing and resending; that the gateway decides nothing (V41) | per change |
| integration | `suite.Run` over `vfs.New` with the real engine on a temporary journal, the in-memory store, real open state and a fake clock, and no node | §5's orders, admission on every entry path, the cross-protocol rules and the V-rows of §6; with fault injection below the real components (a key-value store under the metadata store, RFC 16's `KV`; a remote tier and a small journal under the engine, RFC 8; `RecordingCallbacks` that stall or fail; the fake clock); a model of lock, write, open and unlink interleaved under `-race`; the fuzz targets below | per change; 10⁴ interleavings and each fuzz target for 10 minutes nightly |
| multi-node | two `node.New` over an in-memory control-plane store and a loopback `Transport`, with changes driven by writing to the store, and fault injection in the transport: a lost reply, a duplicate send, a stale epoch | `suite.Run` through one node's gateway; the route envelope, the fence and dedup (V4, V11, V41), cursor safety | per change, once the cluster ships |
| e2e | the adapters: `vfstest.Record` and `vfstest.Null` ([RFC 20 §7.2](rfc-20-adapters.md#7.2%20Handlers%20against%20a%20recording%20service)), then the protocol suites | translation only | per change |

The unit tiers make a failure point somewhere: an admission, pseudo-filesystem,
quota or routing bug fails its own package before the suite runs. Running the
same suite through a gateway is what makes "the caller cannot tell a remote
primary from a local one" a checked claim rather than a design intention.

**Entry point.** `vfstest/suite.Run(t, newService)` runs the conformance suite
against any factory, a filesystem or a gateway, one file per area of §3.1 — clients, names, attributes,
data, locking, shares — plus `crossproto.go`, the cross-protocol scenarios,
driven by `Call` values of different `Protocol` with no adapter, and
`faults.go`, as `pkg/metadata/storetest` does for stores. It is how the
service is tested end to end before any adapter calls it. `Record`, `Null` and
`RecordingCallbacks` in `pkg/vfs/vfstest` are hand-written: `Record` implements
every method explicitly, embedding nothing, so a method added to `Service`
fails to compile until `Record` records it.

**Fuzzing.** Every input a client chooses has a `testing.F` target, seeded
with the golden vectors and the suite's own values:

- **IDs** (`pkg/vfs`): each codec of §3.4 decodes any bytes without panicking
  or allocating more than the encoded size, and an accepted value re-encodes to
  the same bytes;
- **cursors**: a `ReadDir` cursor from arbitrary bytes, or from another
  directory, is refused or resumes inside the directory it names, never
  repeats or skips an entry of a directory left unchanged, and never yields one
  from elsewhere;
- **handles**: arbitrary handle bytes are refused before resolution or admitted
  for their own share only (§4.9, V39); a pseudo handle decodes to a node of the
  pseudo-filesystem or is refused;
- **names**: arbitrary names to `Create`, `Lookup`, `Link` and `Rename` are each
  refused with nothing changed (`ErrNameTooLong`, `ErrInvalid`) or, accepted,
  found once by `Lookup` and once by `ReadDir`;
- **sequences**: the model's operations are decoded from the fuzz input,
  so a failing interleaving is saved to the corpus and replays per change.

**Architecture check.** Beside [RFC 20 §7.1](rfc-20-adapters.md#7.1%20Imports%20and%20call%20sites)'s
test, which covers adapters, a test parses `internal/vfs` and fails, naming the
function or field, on:

- a call from `gateway.go`, `serve.go` or `envelope.go` into the store, the
  engine, open state or identity;
- a field, parameter or conversion of type `*Filesystem` in the gateway or `Serve`:
  they hold a `vfs.Service`;
- an import of network code anywhere under `internal/vfs` (`net`, `net/http`,
  `crypto/tls`, an RPC framework): `Router` and `Transport` are implemented
  outside it;
- a call of `Authorize` or `Granted` outside `authz`;
- any store, engine or open-state import in `admit`, `pseudofs` or `lockorder`,
  which reach the store only through the narrow reads they declare;
- any use of RFC 16's `ControlPlane` view anywhere under `internal/vfs`: it is
  the management API's, and the filesystem reads only the views it declares
  (§10 item 4).

It (V32), and §9's allowlist test, is seeded with one deliberate violation of
each rule in its own test data and must report it.

**Benchmarks.** Driven by the same factory, so a protocol benchmark minus a
service benchmark is the adapter's own cost:

- per operation, ns and allocations: `GetAttr`, `Lookup`, `Read` and `Write` of
  4 KiB and 1 MiB, `Open`, a `ReadDir` page; **0 allocations** on the data path;
- service overhead against the direct store or engine call, and
  **(cluster)** the same call through the gateway, local and over the loopback
  transport;
- contention: N writers on one file, N openers on one file, N creates in one
  directory (the lock order and fold admission), and **(cluster)** quota
  reservation under `k` primaries;
- admission: a cached and an uncached admission, so the cache's key is shown to
  cost less than the checks it saves;
- recall latency: a conflicting write to its acknowledgement, and, on the fake
  clock, to a forced break at the break deadline.

| Tier ([RFC 29 §4.3](rfc-29-test-harness.md#4.3%20Tiers), [§4.10](rfc-29-test-harness.md#4.10%20Benchmarks%20and%20long%20runs)) | What |
| --- | --- |
| per change | each benchmark for one iteration, as a smoke test |
| after merge | `benchstat` against the stored baseline |
| nightly, dedicated host | tracked, as [RFC 29 §4.10](rfc-29-test-harness.md#4.10%20Benchmarks%20and%20long%20runs) says |
| manual | contention sweeps and long model runs |

## 9. Migration

The service is introduced in slices, each one PR or a few, each leaving the
tree working:

> decision: the filesystem is built over the RFC 16 store and RFC 14's open state
> from slice 1, not over today's `metadata.Service` and lock manager, because
> both are deleted when the new store lands and a filesystem delegating to them
> would be written twice. Slice 1's delegation and slice 3's shim are
> therefore skipped, and slice 0's types come from the new store. Today's code
> gets only the drift fixes below. Overturned if the new store slips far
> enough that adapters need the service before it lands.

| Slice | What moves | What is deleted or closed |
| --- | --- | --- |
| 0 | the neutral value types (§3.0) and errors out of `pkg/metadata` into `pkg/vfs`; the allowlist test below, seeded with every adapter file importing `pkg/metadata` (112 today) | nothing yet: the list only shrinks from here |
| 1 | `vfs.Service` and `vfs.New` with its lifecycle (§2.5), the filesystem delegating to today's `metadata.Service`; `vfstest`'s `Record`, `Null` and `RecordingCallbacks`; the integration tier's harness | nothing: this is the harness every later slice tests against |
| 2 | shares, identity and admission: `Root`, `SecInfo`, `ListShares` and the pseudo-filesystem into the service; a cross-protocol test that a quiesced or netgroup-restricted share is refused on every entry path | the four admission paths and three auth-context builders, `v4/pseudofs`, `shareEntryStatus`, the NFSv3 permission cache; closes #3040 and E-01. An adapter's own admission check stays until every operation on its path calls the service: removed earlier, it fails open |
| 3 | the filesystem reaches today's lock manager through RFC 14 §11.3's seven views, served by a shim over the manager, so `Write`'s `CheckIO` and every other open-state call go through one seam. RFC 14's open state is built new, and its `*openstate.Table` then replaces the shim behind the same views | nothing yet; `pkg/metadata/lock` is deleted once the table replaces it, never ported, and none of its lock persistence carries forward |
| 4 | `Write`, `Read` and `Commit`, with the attempt deadline and the write verifier — the four copies of the write sequence become §5.1, returning `WCC` | the adapters' copies and the payload-scope call sites |
| 5 | `Open` and `Close`: deny modes, delete on close, releases | the six deny-mode sites; closes E-08 and #2927 |
| 6 | range-list `Lock` and `Unlock`, then `Copy` and `Clone` | the copychunk loops, `common/clone.go` |
| 7 on | the namespace, attributes and the rest; then [RFC 20](rfc-20-adapters.md)'s shared pipeline and error tables | each adapter's copy as its last caller moves; the per-protocol request plumbing |
| cluster | `vfs.NewGateway`, `vfs.Serve`, and the router and transport behind them, with the multi-node tier (§8) | nothing: a single node is unchanged |

- **The RFC moves with the code.** A slice that needs a signature change edits
  this RFC in the same PR, and a reviewer rejects code that disagrees with it.
- **Copy stays synchronous** ([§5.9](#5.9%20Copy%20and%20clone)).
- **Each slice lands with its tests.** Its §8 integration cases, and a
  system scenario for the cross-protocol rule it moves, turning green in the
  same PR.
- **Drift bugs that lose data or open access are fixed now**, in today's code:
  #3040, #2927 and the write verifier. Each fix's test is written against the
  behaviour, not the handler, so it is reused as a service case when its slice
  lands. The structural ones (deny modes across protocols, change notify across
  protocols, case-insensitive create) are fixed by their slice.
- **Cross-protocol tests run on every PR that can break them.** The
  cross-protocol e2e group and the scenarios of the slices landed so far run
  in [RFC 29](rfc-29-test-harness.md#4.3%20Tiers)'s `pr` tier, with `paths`
  naming the adapters, `internal/vfs`, `pkg/vfs` and `pkg/metadata`; after
  merge or nightly alone, a red one waits days for a reader.
- **The chokepoint shrinks visibly.** A test lists every adapter import of the
  metadata store, the block store and the lock manager, and every direct
  `Authorize` call, against a checked-in allowlist. It fails on a new entry and
  on a stale one, so the list only shrinks and each PR shows its deletions. At
  empty it is
  replaced by [RFC 20 §7.1](rfc-20-adapters.md#7.1%20Imports%20and%20call%20sites)'s
  import test and §8's architecture check.

> decision: slices, each with its RFC edit, and a shrinking allowlist.
> Rejected: one up-front amendment of every RFC before code, which goes stale
> before the first slice lands; leaving the signatures to the implementing
> issue, where no reviewer reads them; and code review alone, or the final
> import test alone, neither of which shows progress or stops a new leak in
> the meantime. Overturned if two slices are found to need landing together.
>
> decision: fix the drift bugs that lose data or open access now, the rest with
> their slice. Rejected: fixing every drift bug now, which patches the copies
> the migration deletes; and fixing none until the service, which leaves a
> known data-loss bug open for the length of the migration.

## 10. Open questions

1. **Compound batching.** Adapters run compounds as sequences of calls. A batch
   call (a `LOOKUP`-`GETATTR`-`OPEN` chain in one service call) pays only across
   the network, since collocated calls are function calls.

   > decision: no batch call now. Rejected: a batch call before it is
   > measured; an adapter cache of service answers, which
   > [RFC 20 §2.2](rfc-20-adapters.md#2.2%20What%20an%20adapter%20may%20hold)
   > forbids. Overturned by §8's benchmark through the gateway over the
   > loopback transport showing compound round trips limiting a cluster.
2. **pNFS layouts.** Not a parity requirement, and not in the service: a
   single node advertises none, so every NFSv4.1 layout operation is answered
   `NFS4ERR_NOTSUPP` from the adapter's table with no service call
   ([RFC 21 §2.1](rfc-21-nfs.md#2.1%20Versions%20served)). When a pNFS RFC lands, a separate `Layouts` interface joins the
   service, its calls over RFC 14's layout state; a layout is bound to the
   (shard, epoch) it was granted under ([RFC 14](rfc-14-open-state.md)).
3. **ID widths.** Settled: [RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references)
   fixes the stateid's and the SMB `FileId`'s shard, instance and `OpenID`
   widths (§3.4).
4. **Gaps in the layers below**, found by §3.5's table: RFC 7's `Attrs` has no exclusive-create
   verifier though [RFC 7 §2.10](rfc-7-namespace-metadata.md#2.10%20Exclusive%20create) stores one, and no call turns an empty file into a
   symlink (SMB `SET_REPARSE_POINT`); RFC 16 declares no read view of quota
   records but `ControlPlane`'s, which the filesystem must not call. Each is the
   owning RFC's to add.

5. **Watches and placement across shards.** A recursive watch over a subtree
   whose directories sit in several shards ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards))
   must be registered at each directory's primary and kept as directories are
   created and moved; who registers them, and what a client is told while one
   is missing, is unspecified. Whether a single node places per-child shards at
   all decides whether `ErrCross` (`OtherShard`) can reach it, and is RFC 11's
   to state.

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
