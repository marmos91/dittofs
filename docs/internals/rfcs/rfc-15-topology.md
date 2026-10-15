---
rfc: 15
title: "RFC 15 — topology and roles"
component: topology
status: deferred
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-10-journal-replication]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
aliases:
  - RFC 15
tags:
  - rfc
---
# RFC 15 — topology and roles

**Status:** deferred. [§11](#11.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone deploying more than one node, adding a component, or
deciding where a call is served.

> [!note] Built after the single-node release
> This design is not shelved. The first release is a single node scaled
> vertically; horizontal scaling is the phase after it, needed for large
> contracts, for large shares spread over several nodes and, later, for
> NFSv4.2 and pNFS. Every rule here is a cluster rule except the sections
> marked **Release 1**, which bind the single node now and which
> [RFC 0's single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)
> cites: the start-up checks ([§2.2](#2.2%20Startup%20refuses%20what%20cannot%20work)), the
> composition ([§2.4](#2.4%20Composition)) and the single-node rule for
> layouts ([§5.1](#5.1%20pNFS)). The profile says what replaces the rest on one node.
> Until the cluster is built only these hooks are implemented:
>
> - the 128-bit content version, with its epoch half held at zero
>   ([RFC 1 §4.3](rfc-1-journal.md#4.3%20Versions));
> - the node epoch and the shard incarnation carried in the NFS write verifier
>   ([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state), rule 3);
> - one binary and one node type ([§2.1](#2.1%20One%20node%20type)).
>
> Adding nodes migrates each journal's format one way, behind a gate
> ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)).

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md).

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

Every node in a cluster is the same: each one talks to clients and each one
keeps files. Each file has one node in charge of it, its **primary**. A call
arrives at whichever node the client is connected to, its **gateway node**. If
that node is the primary, it serves the call itself. If not, it passes the call
on, unopened, in an envelope: a request number, the shard, and the ticket
number it expects the primary to hold. The gateway decides nothing; the primary
decides everything. A primary refuses an old ticket, and answers a repeated
request number with its first answer. The metadata store is not a DittoFS node
at all; every node calls it. On one node there is no gateway: the adapters call
the filesystem directly.

```text
 client ─► gateway node ──vfs.Service, forwarded──► primary ─► bucket
                                                     │ transactions
                                                     ▼
                                               metadata store
```

One node can run all of DittoFS. Several nodes need to agree on three things:
what each node runs, which node is in charge of each file, and which node a
client's call goes to. This RFC answers them: every node runs everything
([§2.1](#2.1%20One%20node%20type)), a shard's primary is in charge of its files
([§3](#3.%20One%20primary%20per%20shard)), and the node a call arrives at
forwards it there ([§2.3](#2.3%20The%20gateway%20forwards%2C%20the%20server%20decides)).

In outline: clients reach DittoFS over NFS (Network File System) or SMB (Server
Message Block); writes land in a local journal and are later uploaded to an
S3 (Simple Storage Service) bucket; a metadata store records where everything
is ([RFC 0](rfc-0-data-lifecycle.md)). The first release is one node, where
everything here is a function call and costs nothing; this is the design for
adding nodes after it.

### The problem, in one example

The installation runs as a cluster of five alike nodes, P1, P2, S1, S2 and S3,
over one replicated metadata store. Files are grouped into **shards**, each with
one **primary**: shard A, `profiles/alice/`, has primary S1; shard B,
`profiles/bob/`, has primary S2. alice-pc maps `\\10.0.0.50\profiles`, an
address that belongs to the cluster and is held, for now, by P1, so P1 is
alice-pc's gateway node.

1. **A write.** alice-pc writes 64 KiB into `ODFC_alice.vhdx`. P1 is not A's
   primary, so its gateway looks the file's shard up in its cached copy of the
   shard records and forwards the write to S1 with an **envelope** — a request
   ID unique in the cluster, the shard and the epoch P1 expects, and a hop
   count of zero. At S1 the receive side checks the epoch, and S1's server
   admits, applies and replies.
2. **A lost reply.** The reply is lost and P1 sends the write again under the
   same request ID. Meanwhile another write has landed on the same bytes.
   Applying the retry would overwrite it; S1 instead answers from its table of
   recent results, keyed by request ID.
3. **P1 dies.** Without help, alice-pc would wait for its connection to time
   out, long after the window in which it could reclaim its opens. Instead,
   once P1's node lease has lapsed and every node has fenced P1, so no
   call P1 still has in flight can land, P2
   takes over `10.0.0.50`, announces it on the network, and sends alice-pc a
   TCP acknowledgement that makes it reset and reconnect at once. On a share
   offering continuous availability, the SMB Witness protocol also tells
   alice-pc that the address moved and where to go. alice-pc reconnects to P2
   and finds its durable open, which S1 held all along: losing a gateway node
   loses no open state.
4. **S1 dies.** The primary is lost instead. Shard A fails over to a replica at
   a higher epoch ([RFC 11](rfc-11-ownership.md)), and its clients reclaim their
   opens in a grace period ([RFC 14](rfc-14-open-state.md)). P2's next call
   still names S1's epoch; it is refused, P2 re-reads the shard record and
   retries. A stale route costs one refusal, never a write applied by a node
   that is no longer in charge.

```text
                      alice-pc (SMB)  \\10.0.0.50\profiles
                          │
           10.0.0.50 held by P1 ── P1 lost ──► P2 takes 10.0.0.50,
                          │                    resets alice-pc's TCP,
                          │                    Witness says where to go
                          ▼
  ┌ P1: gateway node for alice-pc ────────────────────────────────┐
  │ adapters, sessions; vfs.Gateway: route by handle, stamp,      │
  │ send, retry — decides nothing                                 │
  └───────┬───────────────────────────────────────────────────────┘
          │ envelope: request P1#4711, shard A, epoch 7, hop 0
          ▼
  ┌ S1 ──────────────────────┐ ┌ S2 ─────────────┐ ┌ S3 ──────────┐
  │ vfs.Serve: fence, dedup  │ │ primary of B    │ │ replica of A │
  │ vfs.Server, primary of A │ │ profiles/bob/   │ │              │
  │ open state, journal,     │ │                 │ │              │
  │ results by request ID    │ │                 │ │              │
  └──────────────────────────┘ └─────────────────┘ └──────────────┘
          shard records: A ─► S1 at epoch 7 (replicated metadata store)
```

Every node runs every box above; P1 is drawn as a gateway and S1 as a primary
only because of where alice-pc connected and which shard her file is in. The
figure draws the same design with three nodes, where S2 is the gateway:

![A cluster of identical nodes S1, S2 and S3, each running adapters, vfs.Gateway, vfs.Serve with vfs.Server, open state, metadata transactions, and the journal, engine, syncer and GC; each is primary of one shard and replica of the others; S2's gateway forwards a write for shard A to S1's server with an envelope, and every node reads and writes one external replicated metadata store and shares one S3 bucket](img/rfc0-cluster.svg)

### The words you need

- **[Node](rfc-0-data-lifecycle.md#Glossary)**: one DittoFS server process.
  Every node runs everything — adapters, the filesystem service, open state,
  journals and the content path ([§2.1](#2.1%20One%20node%20type)).
- **[Shard](rfc-0-data-lifecycle.md#Glossary)** and
  **[primary](rfc-0-data-lifecycle.md#Glossary)**: a set of files, a share by
  default, and the one node that accepts its writes at a time
  ([§3](#3.%20One%20primary%20per%20shard)).
- **[Gateway node](rfc-0-data-lifecycle.md#Glossary)**: the node a client's
  call arrives at. Its `vfs.Gateway` forwards the call to the primary when it is
  not the primary itself, and decides nothing
  ([§2.3](#2.3%20The%20gateway%20forwards%2C%20the%20server%20decides)).
- **[Epoch](rfc-0-data-lifecycle.md#Glossary)**: the fencing number in a shard's
  record; what raises it is stated once, in
  [RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms). A call carrying an
  older one is refused.
- **Route envelope**: what every forwarded call carries: request ID, shard and
  epoch, sending node and node epoch, hop count, trace context ([§4.3](#4.3%20The%20route%20envelope)).
- **Floating address**: a client-facing address owned by the cluster, taken
  over by a surviving node ([§5.3](#5.3%20Client%20addressing)).
- **SMB Witness**: the SMB protocol by which a cluster tells a client that an
  address moved or a node is draining, and where to go
  ([§5.3](#5.3%20Client%20addressing)).

### What this RFC promises

- Every node runs the same binary and the same components.
- Each shard has one primary at a time. Every decision about its files — admission,
  permissions, ordering, open state and writes — is made there, so a conflict check
  and the I/O it admits are never on two nodes.
- A gateway forwards and never decides.
- A call that reaches a node no longer in charge is refused, never applied; a
  change retried through the same gateway node, or replayed by SMB on another
  channel, returns its first result instead of applying twice
  ([§4.3](#4.3%20The%20route%20envelope)).
- When a node is lost, a survivor takes its client addresses and makes its
  clients reconnect at once, and its shards fail over to their replicas.

### Scenarios

**alice opens a file through a node that is not its primary.** P1 holds
alice-pc's connection; S1 is primary of shard A, `profiles/alice/`.

1. alice-pc asks P1 to open `profiles/alice/notes.txt`.
2. Each `Lookup` of the walk names a directory handle; P1's gateway forwards
   each to the directory's primary, S1, and S1's server answers it
   ([§2.3](#2.3%20The%20gateway%20forwards%2C%20the%20server%20decides)).
3. The open is forwarded the same way, and S1's server admits it and records it
   in S1's open state ([§2.5](#2.5%20The%20cut)).
4. Each write goes P1 to S1, then to S1's two replicas, before the OK
   ([§2.5](#2.5%20The%20cut)).

A write crosses the network four times before its OK; had alice-pc connected to
S1 itself, the gateway would have called S1's server directly and saved one
crossing.

**Draining a node for maintenance.**

1. An operator drains P1, which holds the address `10.0.0.50`
   ([§5.3](#5.3%20Client%20addressing)).
2. NFSv4 clients are given another node's location, and move before P1
   stops ([§5.3](#5.3%20Client%20addressing)).
3. SMB clients registered for Witness are told which node to move to
   ([§5.3](#5.3%20Client%20addressing)).
4. P1 stops forwarding, then releases its addresses; NFSv3 clients follow the
   address. A drain needs no fencing wait ([§5.3](#5.3%20Client%20addressing)).
5. P1 hands the shards it is primary of over to their replicas before it stops
   ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)).

No client waits for a transport timeout, and no open state is lost: the
primaries hold it, and a handover ships it.

**A rename across two shards.**

1. A user moves `profiles/alice/report.docx` into `profiles/bob/`, in shards A
   and B ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)).
2. The coordinator is the source directory's primary, S1. It asks B's primary,
   S2, to prepare ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)).
3. S2 checks open state, recalls grants the rename breaks, writes a durable hold
   record and answers yes ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)).
4. S1 commits one store transaction guarding both shards' fences and S2's hold
   record ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)).
5. S2 drops its hold. Had S1 vanished, S2's hold would end at its deadline
   ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)).

One rename, two primaries, no coordinator log. The file stays in shard A: a
rename never changes a file's shard ([RFC 11 §2](rfc-11-ownership.md#2.%20Shards)).

**A clone across shards becomes a copy.**

1. An NFSv4.2 client asks to `CLONE` a file in shard A into shard B
   ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)).
2. Source and destination are in different shards, so admission refuses the
   clone ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)).
3. The adapter answers `NFS4ERR_XDEV`, never `NFS4ERR_NOTSUPP`, so the client
   keeps cloning within one shard ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)).
4. A `COPY` of the same pair runs as a server-side byte copy
   ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)).

The bytes are right; only the speed of a clone is lost.

**The metadata store stalls for 15 s.**

1. The store is unreachable for longer than a node lease, 10 s by default
   ([§5.3](#5.3%20Client%20addressing)).
2. When P1 and P2 reach it again, each sees the other's lease apparently lapsed.
3. Neither marks the other lapsed until it has held the claim hold after
   reconnecting ([§5.3](#5.3%20Client%20addressing)).
4. Both resume, and no address moves ([§9](#9.%20Conformance%20and%20benchmarks)).

A store outage does not turn into a storm of address takeovers.

### How the rest is organised

[§2](#2.%20Roles) defines the one node type, the start-up checks, what the
gateway and the server each do ([§2.3](#2.3%20The%20gateway%20forwards%2C%20the%20server%20decides)), how a node is built, and why
the cut is at `vfs.Service` ([§2.5](#2.5%20The%20cut)). [§3](#3.%20One%20primary%20per%20shard) is the one-primary rule.
[§4](#4.%20Where%20each%20call%20runs) says which node serves each call, what
makes a call safe to route, how calls spanning two shards run, and the
envelope. [§5](#5.%20Clients) covers clients: pNFS (parallel NFS), SMB and
NFSv3, and client addressing. [§6](#6.%20Learning%20primaries) is how gateways
learn primaries, [§7](#7.%20High%20availability%20needs%20no%20split)
why availability needs no split, and [§8](#8.%20Invariants) the invariants. On
a first read, skip §2.4, §4.2 and §5.1.

## 1. Purpose

One node runs everything. Several nodes need to know which one owns each file,
and where each call goes. This document answers:

> **What does each node run, who owns each file, and which node serves each
> call?**

### 1.1 Non-goals

This document **MUST NOT**:

- define how a shard's primary is chosen, fenced, moved or failed over — [RFC 11](rfc-11-ownership.md);
- define journal replication — [RFC 10](rfc-10-journal-replication.md);
- define open state — [RFC 14](rfc-14-open-state.md). It says only which primary holds it;
- define a wire protocol between nodes. It states what every call must satisfy
  to cross one ([§4.1](#4.1%20Every%20call%20is%20safe%20to%20route)).

## 2. Roles

### 2.1 One node type

Every node runs the same binary and composes the same components, so any node
can hold a client's connection, be a shard's primary, or be its replica:

| Component | What it does on every node | Specified in |
| --- | --- | --- |
| adapters | sign clients in with the authenticator ([RFC 18](rfc-index.md)), speak the wire, hold sessions; call `vfs.Service` and nothing else | [RFC 20](rfc-20-adapters.md) |
| `vfs.Gateway` **(cluster)** | routes each call to the primary of its file's shard; decides nothing | [§2.3](#2.3%20The%20gateway%20forwards%2C%20the%20server%20decides) |
| `vfs.Serve` **(cluster)** | receives forwarded calls: fences and deduplicates them, then calls the local server | [§2.3](#2.3%20The%20gateway%20forwards%2C%20the%20server%20decides), [§4.3](#4.3%20The%20route%20envelope) |
| `vfs.Server` | the filesystem: decides every call for the shards this node is primary of | [RFC 17](rfc-17-vfs.md) |
| identity, open state | map identities per share; hold opens, locks, caching grants and layouts | [RFC 18](rfc-index.md), [RFC 14](rfc-14-open-state.md) |
| content subsystem | engine, journals and their replication, syncer, GC | [RFC 8](rfc-8-engine.md), [RFC 10](rfc-10-journal-replication.md) |
| client-address agent; **(cluster)** pNFS metadata-server and data-server endpoints | floating addresses and SMB Witness; layouts | [§5](#5.%20Clients) |
| control plane, management API | settings, shares, principals | [RFC 13](rfc-13-configuration.md) |

Every node therefore needs the metadata store, journal devices and remote-tier
credentials. The default and only supported deployment is this one node type: a
single node, or several alike over one replicated metadata store. The `protocol`
and `storage` roles survive only as one configuration flag recorded on the node
record ([RFC 13](rfc-13-configuration.md)); both is its only accepted value
([§2.2](#2.2%20Startup%20refuses%20what%20cannot%20work)), and no rule, test or
document of this set designs a node with one.

> ponytail: every node runs protocol and storage. Split the roles when protocol
> CPU (TLS/SMB encryption, GSS) is measured saturating nodes whose disks are
> idle, or when a deployment needs disk-less protocol nodes for isolation. The
> split costs only `vfs.Gateway` with no local server; no interface changes.

> ponytail: one primary caps a shard's metadata operations and data bandwidth
> at one node together ([RFC 11 §2.1](rfc-11-ownership.md#2.1%20One%20primary%20per%20shard)). The upgrade is a namespace primary and a data
> primary per shard. Take it on measured metadata/data contention on one
> primary node, or once separately scaled pNFS metadata and data servers are
> committed to.

### 2.2 Startup refuses what cannot work

> [!important] Release 1
> This section binds the single-node release; [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) cites it.

A node **MUST** refuse to start when:

- its roles flag names one role only ([§2.1](#2.1%20One%20node%20type));
- it uses an embedded single-node metadata store and is not the only node;
- it has no journal device or no remote-tier credentials.

### 2.3 The gateway forwards, the server decides

The filesystem service ([RFC 17](rfc-17-vfs.md)) is one interface,
`vfs.Service`, which adapters call with values only. In a cluster each node
holds three things behind it, all in package `vfs`:

| Piece | Does | Never does | Holds |
| --- | --- | --- | --- |
| **`vfs.Gateway`** — implements `vfs.Service` | resolves the routing handle each request value names to its shard's primary through `vfs.Router` ([§6](#6.%20Learning%20primaries)); if the primary is this node, calls the checked `vfs.Service` that `vfs.Serve` returns; otherwise stamps the route envelope ([§4.3](#4.3%20The%20route%20envelope)) with the request identity the adapter handed it, sends the call through `vfs.Transport`, and retries, traces and counts it — one generic call path for every method | authenticate, admit to a share, map an identity, squash, check a permission, order, parse protocol data, or call open state, the metadata store or the engine itself | nothing about files; its router caches shard records and files' recorded shards |
| **`vfs.Serve`** — the receive side | refuses a call whose node epoch is fenced, whose shard epoch is not current or whose hop count is not zero, naming the primary it knows; answers a retry from the dedup table by request ID ([§4.3](#4.3%20The%20route%20envelope)); then calls the server. It runs the same checks for a call that arrives over `vfs.Transport` and for the gateway's local path, which it returns as a checked `vfs.Service` | forward a call, or read anything about the file | the dedup table |
| **`vfs.Server`** — the filesystem | everything else: refusing a file whose recorded shard is not the one named (`ErrWrongShard`), admission to the share, identity resolution, flavour and squash, ordering, open state, metadata and content ([RFC 17](rfc-17-vfs.md)) | route | open state, quota reservations, grace, admitted-call counts |

So adapters sign in, the gateway forwards, and the server decides. The gateway's
one choice — which node to send to — decides nothing either: a wrong choice is
refused by the receiver, never applied ([§6](#6.%20Learning%20primaries)). The
router's reads of shard records and of a file's recorded shard are routing, not
file logic, and are made by its implementation outside `vfs`.

```text
 adapter ─ signs in, decodes, calls vfs.Service
    │
    ▼
 vfs.Gateway ─ route by handle (vfs.Router over shard records)
    │
    ├── primary is this node: the checked vfs.Service ────────────┐
    │                                                             │
    └── primary elsewhere: stamp envelope ─► vfs.Transport        │
                (retry, trace, metrics)        │ node channel     │
                                               ▼                  ▼
                                 vfs.Serve at the primary, for both paths:
                                 fence (node epoch, shard epoch, hop 0),
                                 dedup by request ID
                                               │
                                               ▼
                                vfs.Server at the primary: admission,
                                identity, squash, ordering
                                ─► open state, metadata, engine
```

- The gateway **MUST** hold a `vfs.Service`, never a `*vfs.Server`, so its local
  shortcut and its remote path are the same interface.
- In a cluster the local shortcut **MUST** pass the same fence and dedup as a
  forwarded call — `vfs.Serve`'s checks, run in-process — and carry the same
  request ID, so a call a stale cache routes locally is refused, and the retry
  of a call first sent locally is answered once, after a takeover too. The
  shortcut drops only the network hop: `vfs.Serve` returns its checks as a
  checked `vfs.Service`, and that, never the bare server, is the gateway's
  local `vfs.Service`.
- The request identity a request ID is derived from ([§4.3](#4.3%20The%20route%20envelope)) — XID,
  procedure and argument checksum, or session, slot and sequence — is the
  adapter's, which hands it with the call; the gateway only stamps it.
- `vfs.Router`, `vfs.Transport` and `vfs.Peers` **MUST** be declared in `vfs` and
  implemented outside it, so `vfs` imports no network code.
- A call names its route by its handle, or, with none, by the shard its
  grant, watch or state ID names
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)). A call with neither — `Root`,
  `ListShares`, the pseudo-filesystem, a client's lease and session calls —
  runs at the gateway node's own server. A client's lease owner and back
  channel are chosen among the nodes holding its sessions
  ([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)), so renewal
  costs no hop when the owner is this node.
- A call that touches two shards is routed by its first handle to its
  coordinator, the source directory's primary or, for a link, the target
  directory's, whose server runs it as [§4.2](#4.2%20Calls%20that%20touch%20two%20primaries) says. A path walk is one `Lookup` per
  component, each routed by its directory. The gateway never splits a call.
- A single node builds no gateway and no receive side: its adapters call
  `vfs.Server` directly, and none of the envelope, fencing or dedup of a
  forwarded call applies.

> decision: a call naming no file is decided by the gateway node's own
> `vfs.Server`, not at a primary, because it touches no shard's state: share
> admission there reads the share records any node reads, and every later call
> on the share's files is admitted again by its primary's server. Overturned if
> such a call ever needs state a primary holds.

### 2.4 Composition

> [!important] Release 1
> This section binds the single-node release, which builds every step below
> except the cluster's; [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) cites it.

One type, `node.Node`, built by `node.New(cfg)`, composes a node. No other
component builds components: each is built from its `Deps` and is handed what it
needs. `New` first refuses what [§2.2](#2.2%20Startup%20refuses%20what%20cannot%20work) lists, then builds, in this order, and
starts nothing:

| Step | Builds | Calls |
| --- | --- | --- |
| 1 | the metadata store, embedded or a client of the replicated one ([RFC 16](rfc-16-metadata-store.md)) | — |
| 2 | the remote stores ([RFC 4](rfc-4-remote-tier.md)) | — |
| 3 | the syncer ([RFC 3](rfc-3-syncer.md)) | the remote stores |
| 4 | the engine ([RFC 8](rfc-8-engine.md)), handed the journal opener: it opens each device's journal itself at `Start`, and **(cluster)** its replication ([RFC 1](rfc-1-journal.md), [RFC 10](rfc-10-journal-replication.md)) | the journal, the syncer, the Existence, Content and Lifecycle views; the carver's `Cut`, which holds no state and is not built ([RFC 8 §2.1](rfc-8-engine.md#2.1%20Content%20composition)) |
| 5 | GC ([RFC 9](rfc-9-gc.md)), a sibling of the engine, never passed into it | Blocks, the remote stores, the syncer, Keys |
| 6 | open state ([RFC 14](rfc-14-open-state.md)) | — |
| 7 | identity ([RFC 18](rfc-index.md)) | — |
| 8 | `vfs.New`, the `*vfs.Server` ([RFC 17 §2.3](rfc-17-vfs.md#2.3%20Dependencies%20and%20wiring)) | identity, open state, the metadata views, the engine |
| 9 | **(cluster)** `vfs.Serve` over the server, then `vfs.NewGateway` with the checked `vfs.Service` `vfs.Serve` returns as its local one, the router ([§6](#6.%20Learning%20primaries)) and the transport | Serve: the server; the gateway: the checked `vfs.Service`, `vfs.Router`, `vfs.Transport` |
| 10 | the adapters ([RFC 20](rfc-20-adapters.md)), handed the `vfs.Service` — the gateway in a cluster, the server on one node — and identity as their authenticator | `vfs.Service` |

`Node.Start` opens the metadata store and registers the node first, so a
version refusal stops it before anything else starts; then starts the other
components in build order, except the adapters; runs the first reconcile; and
only then starts the adapters: no adapter listens until the node's shares
are open and, in a cluster, the node holds a node epoch to stamp. `Node.Stop`
stops them in reverse: adapters first, so no client call arrives at a component
already closed; then **(cluster)** the gateway; then the node hands the shards
it is primary of over
([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) while
`vfs.Serve` still receives, so other nodes' calls to them are answered, not
refused until a failover; then `vfs.Serve`, and GC drains before the content
subsystem closes. No component closes anything it was
handed. [RFC 8 §2](rfc-8-engine.md#2.%20Composition) composes the content
subsystem only; this section is the root above it.

> ponytail: the node both builds and drives its components. Split the builder
> out when it gains dependencies a multi-node test cannot fake cheaply.

### 2.5 The cut

A client call crosses between nodes at exactly one interface: `vfs.Service`,
forwarded whole by the gateway and received by `vfs.Serve`
([§2.3](#2.3%20The%20gateway%20forwards%2C%20the%20server%20decides)). Below
it, everything that call touches runs at the primary; above it, nothing holds
file state. This binds every deployment; on a single node the cut is a function
call.

![The cut is vfs.Service: on the gateway node an adapter signs in and calls vfs.Gateway, which decides nothing; the gateway either calls the local vfs.Server or stamps an envelope (request ID, shard and epoch, node and node epoch, hop 0) and sends it over vfs.Transport to vfs.Serve at the primary, which checks the fence and dedup; vfs.Server at the primary then decides admission, identity, squash and ordering and calls open state, the metadata store and the engine, while the legend gives the reasons the cut is not under the VFS](img/rfc15-cut.svg)

**Why the cut is at `vfs.Service` and not under it.** Forwarding the engine's,
the metadata store's or open state's calls instead would keep the filesystem
service at the gateway and cost:

| Cut under the filesystem service | Cut at `vfs.Service` |
| --- | --- |
| open state at the gateway for admission and at the primary for I/O: two copies, and a grace period that spans both | open state in one place, the primary's server |
| a `WRITE` is several forwarded calls — admit, check open state, reserve quota, append, update size and times | a `WRITE` is one forwarded call |
| the engine interface is chatty: calls per extent, per version and per commit, each a round trip | one round trip per client operation |
| each forwarded piece fenced on its own, so a check and the I/O it admits can straddle an epoch change | one epoch, checked once by `vfs.Serve`, fences the whole call, and the server runs the check and the I/O under it |

**What else crosses between nodes.** Every message travels on a node channel
([§4.4](#4.4%20Node%20channels)):

| Message | Between | Defined in |
| --- | --- | --- |
| a forwarded `vfs.Service` call | a gateway and `vfs.Serve` at the primary | §2.3, [§4.3](#4.3%20The%20route%20envelope) |
| a replication message | a primary and its replicas | [RFC 10](rfc-10-journal-replication.md) |
| a cross-shard prepare, commit or release | two primaries' servers, through `vfs.Peers` | [§4.2](#4.2%20Calls%20that%20touch%20two%20primaries) |
| a server's other call to a second primary: an overlay of an entry in another shard, a cross-shard copy's source read, an NFSv4.0 owner's sequence at its home primary, the lease owner's calls to its client's primaries, and their recalls back | two servers, through `vfs.Peers` | [RFC 17 §2.3](rfc-17-vfs.md#2.3%20Dependencies%20and%20wiring) |
| a handover's or move's shipped tail and open-state table | two primaries | [RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries) |
| a newest-version question for a read served off the primary | a node and the primary | [RFC 11 §6](rfc-11-ownership.md#6.%20Reads%20on%20other%20nodes) |

**Metadata logic runs at the primary; metadata lives in the store.** RFC 6 and
RFC 7 define records and the transactions that change them. A shard's primary
runs those transactions; the store only keeps and orders them, and is no
DittoFS node ([RFC 11 §1](rfc-11-ownership.md#1.%20Purpose)). A node writes a
file's, a directory's or a shard's record only as that shard's primary. Besides
those, a node writes its own node records — its lease, and the lapsed mark on a
lost node before taking its addresses ([§5.3](#5.3%20Client%20addressing)) —
the health condition it records from its gateway's failed forwards to a primary it cannot reach
([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Gateways%20forward%20to%20the%20primary)),
the client records it is lease owner of ([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)),
and control-plane records through `ControlPlane.*`.

**What a write crosses.** A client write that reaches a node which is not its
shard's primary crosses the network four times before it is acknowledged —
client to gateway node, gateway node to primary, primary to each of two replicas
at the default count ([RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement)) — and once more
after, primary to the remote tier. A client connected to the primary's own
node, or holding a pNFS layout, skips the second crossing. A node's network is
therefore sized for about four times its shards' client write rate.

## 3. One primary per shard

[RFC 11](rfc-11-ownership.md) gives each shard **one primary**, a node named by the
shard record as (node, node epoch) with one shard epoch, and live while that
node lease is. The primary serialises everything that acts on
the shard's files: namespace writes, open state and locks, caching grants,
layouts, journal appends, existence commits, offload, removals, releases and
clone. A file is never split across shards: per-file and range shards are
deferred to [RFC 11 Appendix C](rfc-11-ownership.md#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards).

Because the conflict check of an I/O and the I/O itself run at one primary, no
grant can land between them and no I/O pays a hop to be checked. `size`,
write-time times, `LAYOUTCOMMIT`, truncate and release are all local to the
primary: nothing within one shard crosses between nodes.

## 4. Where each call runs

The gateway routes each `vfs.Service` call by its handle to that shard's
primary ([§2.3](#2.3%20The%20gateway%20forwards%2C%20the%20server%20decides)); adapters never see this table. The table says which
calls of the metadata store ([RFC 16](rfc-16-metadata-store.md)) a server may make for a shard it
is not primary of:

| Made by | Calls |
| --- | --- |
| **any node's server** (store reads, needing no primary) | `Lookup`, `Entries`, `EntriesPlus`, `Files.Get` (the committed part), `ACL`, `Xattrs`, `Streams`, `Authorize`, `Resolve`, `Capacity.*`, `Principals.*` |
| **any node** (store transactions, no shard's primary) | `ControlPlane.*` |
| **the shard's primary** | `Create`, `Link`, `Unlink`, `Rename`, `SetAttrs`, `SetACL`, `SetXattr`, `RemoveXattr`, `OpenState.*`, `Existence.*`, the size and times overlay of `Files.Get`, reads and writes |
| **any node, by sweep assignment** | `Blocks.*` ([RFC 9](rfc-9-gc.md)) |

A read off the primary is deferred: the gateway forwards every read to the primary ([RFC 11 §6](rfc-11-ownership.md#6.%20Reads%20on%20other%20nodes)).

### 4.1 Every call is safe to route

For a call to cross a network unchanged, every interface method **MUST**:

- take and return values, never shared pointers or handles to store state;
- resume an iteration from a cursor the caller holds, never from iterator state
  kept by the server;
- carry the route envelope ([§4.3](#4.3%20The%20route%20envelope)) when it is routed to a primary, so a call
  reaching a former primary is refused rather than applied, and a retry after a
  lost reply is answered rather than applied twice.

Collocated, the same interface is a function call; the caller cannot tell which
it has. Lifecycle methods — start, stop, health, statistics — are not routed
calls and stay out of the routed interfaces.

### 4.2 Calls that touch two primaries

A rename, link or unlink can touch files in several shards, and a create whose
new file lands in a different shard from its directory touches two. Each runs
as [RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards) gives it:

- **One coordinator** runs it: the source directory's primary, or, for a link,
  the target directory's.
- **Prepare.** The coordinator asks, through `vfs.Peers`
  ([RFC 17 §2.3](rfc-17-vfs.md#2.3%20Dependencies%20and%20wiring)), the primary of every other shard involved to
  check the operation against its open state, recall the caching grants it
  breaks, and hold the files until an outcome or a deadline in store time, no
  later than its own node lease expiry less the drift bound, and first commit a
  durable hold record for the operation. Each participant draws, under each of
  its files' accept locks, the version that file's change takes from its own
  journal ([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)),
  and answers yes with those versions, its (shard, epoch) and (node, node
  epoch), or no.
- **Commit.** On every yes, the coordinator commits **one** metadata-store
  transaction that guards the fence records of every file and directory it
  changes at the (shard, epoch) each primary answered with, guards each
  participant's shard record and node record at what it answered, and guards
  each participant's hold record, committing only if every one still exists.
  No time check decides it. On any no, it aborts.
- **Release.** The coordinator sends the outcome, and each primary drops its hold
  and deletes its hold record. A participant whose deadline passes first
  releases on its own by a transaction deleting its hold record, and grants
  nothing the hold refused until that has committed, so a late commit conflicts
  with the deletion or finds the record gone, and aborts.

**The coordinator does not decide a release it cannot see.** Whether a file with
no entries left may be released depends on its opens, which only the file's own
primary holds. A rename over a target, or an unlink, whose file is in a shard the
coordinator is not primary of **MUST** always write the file's pending release in its
transaction ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)), and the file's primary decides: it releases once no
open holds the file, and after grace if it is in one.

**The file's primary makes its opens durable at prepare.** The coordinator's
transaction cannot write open records it cannot see, so the participant that is
the file's primary does it: when it prepares a removal of the file's last entry,
it **MUST** commit a durable open record for each of the file's opens before it
answers yes, and while its hold stands it **MUST** write the durable open record
of any open of that file it admits, in that open's own transaction. The
coordinator's commit guards the hold record, so the unlink commits only while
that rule is in force; once it has, the file has no entry and an open of it
follows [RFC 7 §4.5](rfc-7-namespace-metadata.md#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction). If the outcome is an abort, the durable records written for a
linked file are deleted at their close like any other; they keep nothing alive
that its entry does not.

**(cluster)** **A clone across shards is refused, not coordinated.** The calls
above are the ones [RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards) defines; copy and clone are not among
them. A clone keeps a registration at the source's primary that freezes the
source extent until the clone ends, and that registration is rebuilt after a
failover only from the source's clone-kind open record, which names the
destination and the clone's version ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)). A
by-reference clone or copy whose source and destination are in different
shards **MUST** therefore be refused at admission with `ErrCross` (`CrossShard`)
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)), whatever their primaries are then: two shards
that share a primary at admission can be split by the next failover or
handover mid-clone. A clone admitted within one shard is not split by a batch
move either: a batch skips a file that is the source or destination of an
unfinished clone until the clone is done
([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)). On
`ErrCross` (`CrossShard`) the adapters fall back without telling the client the
operation is missing: an NFSv4.2 `COPY` and an SMB copychunk become a
server-side byte copy, reads at the source's primary through `vfs.Peers` and
writes at the destination's; an NFSv4.2 `CLONE` answers `NFS4ERR_XDEV`, never
`NFS4ERR_NOTSUPP`, which a client records as the operation missing for the
whole mount; an SMB `FSCTL_DUPLICATE_EXTENTS_TO_FILE` answers
`STATUS_NOT_SAME_DEVICE`.

> ponytail: a cross-shard clone costs a byte copy. Add copy to the calls above,
> a prepare at the source's primary that freezes the extent and commits the
> clone-kind open before it answers, when reflinks across per-child shards show
> up as copy time on a real workload.

### 4.3 The route envelope

Every call a node forwards to a primary carries one envelope:

| Field | Means |
| --- | --- |
| request ID | derived from the protocol's own retry identity, not from the connection, because an NFSv4 client retries only on a new connection (RFC 7530 §3.1.1) and an NFSv3 one may: for NFSv3 and NFSv4.0, (client address, XID, procedure, checksum of the arguments), as a server's reply cache keys a retry; for NFSv4.1, (gateway node's ID, the node epoch the session was created under, session, slot, sequence ID), since each gateway node mints its own session IDs and nothing makes them unique across gateway nodes. Whatever its fields, a request ID is stored as one byte string ([RFC 16 §4.2.1](rfc-16-metadata-store.md#4.2.1%20Key%20encoding)) whose first byte tags its family — NFSv3, NFSv4.0, NFSv4.1 or SMB — so IDs of two families, whose fields can encode alike, never compare equal. The epoch is the session's, not the gateway node's current one: a gateway node marked lapsed re-acquires a higher node epoch without restarting, its sessions survive, and a retry on the same session, slot and sequence must still carry the ID its first send did. A retry carrying the same identity carries the same ID: for NFSv3 and NFSv4.0 on any connection and through any gateway node that holds the client's address; for NFSv4.1 on any connection of its session, which lives at one gateway node ([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Gateways%20forward%20to%20the%20primary)). An SMB request is given an ID the gateway node mints — its node ID and node epoch, the connection, and a number never reused on it — since SMB names its retries by open state instead (below); it is never derived from SMB message IDs, which are per connection, so two channels of one session can send two different requests under one (client GUID, session ID, message ID) |
| shard and epoch | the shard the sender routed by and the primary epoch it expects |
| node and node epoch | the forwarding node and the node epoch of its lease; a primary refuses a call from a node epoch that has been fenced ([§5.3](#5.3%20Client%20addressing)) |
| hop count | zero from the gateway node; a node that is not the primary refuses a call whose hop count is not zero rather than forward it ([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Gateways%20forward%20to%20the%20primary)) |
| trace context | the sender's trace ID, parent span ID and sampled flag (RFC 25 §6.2); never part of the request ID |

- The primary **MUST** refuse a call whose epoch is not its current one for the
  shard ([§6](#6.%20Learning%20primaries)), and one whose sending node epoch it has fenced.
- The primary **MUST** keep a dedup table of recent mutations keyed by the
  request ID alone — not by shard or epoch, so a retry that straddles an epoch
  raise, or is re-routed to the shard its file moved to, is still recognised —
  holding each one's result for at least the **retry window**, and **MUST**
  answer a retry from it rather than apply it again. The retry window is an
  installation setting ([RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)), counted from the
  mutation's acknowledgement, and the journal's request entries and the
  request records expire by it too. The control plane refuses a window below 600 s — a hard-mounting
  client's retransmission back-off cap (600 s on Linux) — plus the takeover
  bound ([RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)); the request
  deadline (30 s) bounds the server's work on a call, not when its client
  retries it, and is no bound for the window. An entry is recorded when its
  mutation is admitted, so a retry that finds it still running waits for its
  result instead of running a second time. A handover ships only the entries
  of open-state calls — an SMB `CREATE`, `CLOSE` or `LOCK`, an NFSv4.1 `OPEN`,
  `OPEN_DOWNGRADE`, `CLOSE`, `LOCK` or `LOCKU`, a sequenced NFSv4.0 operation —
  with the open-state table they apply to
  ([RFC 14 §10](rfc-14-open-state.md#10.%20Shard%20placement)), since those calls leave no durable result to
  find again and a retry run twice would conflict with the client's own lock or
  answer a bad stateid or seqid; they are few next to writes. The rest of the
  table is not shipped, since at gigabytes per window it would stretch the
  freeze past its target: the new primary looks a journal operation's or a
  namespace mutation's retry up on a miss in the journal request entries and
  durable request records, and the files of a batch move carry their own
  entries ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)). Each client's entries are
  capped at the per-client cap ([RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)), the oldest
  dropped first. Expired request records are swept through an expiry index kept
  per shard, `SH‖shard‖rqx‖expiry‖requestID`
  ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), rather than only by a new
  primary's fold: one installation-wide time-ordered range would put every
  namespace mutation's insert at the tail of one key range, and each primary
  sweeps only its own shards' expired entries. A
  write whose reply was lost, retried after another write landed on the same
  extent, then returns its first result instead of overwriting the second.

> decision: a retry inside the retry window is answered once; one later is
> applied again, as on any NFSv3 server, and so is one whose entry its
> client's cap dropped. No finite window covers a client cut
> off for longer than the window, which resends on reconnect whatever the
> back-off, so the window is sized to the clients' retransmission behaviour,
> not as a guarantee. Overturned if client traces show retries arriving after
> the window often enough to matter.

> ponytail: the table keeps every mutation of the window, about 1.8×10⁷ entries
> at 20,000 mutations a second for 15 minutes, bounded only by the per-client
> cap. Bound it by memory, as a server reply cache is, when the handover and
> takeover benchmarks show the table's rebuild or lookups in their times. The
> request records' expiry index is per shard, so a share with one busy shard
> still inserts at the tail of one range; put a hash bucket before the expiry
> when that range shows up as a hot spot in the store's own metrics.
- **The table survives a takeover (cluster).** A journal operation carries its
  request ID into the journal and to every replica, and its record keeps the
  request ID and result ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)); a namespace mutation writes a
  request record with its result in its own transaction
  ([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Gateways%20forward%20to%20the%20primary)). A new primary rebuilds the table from both before it
  serves, so a retry after a failover is answered, not applied again.
- **An SMB replay on another channel is recognised by SMB's own state at the
  primary**, not by request ID. A request the client flags as a replay carries
  a new request ID, and the primary, which holds the open ([RFC 14](rfc-14-open-state.md)),
  matches it as MS-SMB2 does: a create by its create GUID, a lock by the open's
  lock sequence, and every other operation on an open by the open's channel
  sequence, refusing with `STATUS_FILE_NOT_AVAILABLE` a `WRITE`, `SET_INFO` or
  `IOCTL` whose channel sequence is stale, or that replays one still in flight
  under an earlier sequence — never holding it — and no other request
  (MS-SMB2 3.3.5.2.10).
  The channel sequence and the counts of requests outstanding under it are
  fields of the `Open` record ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), held at the primary and handed
  over with it. For a persistent open the channel sequence is durable, written
  in the transaction that applies the request, so a stale replayed write is
  refused after the loss of a gateway node and after a failover alike; the
  outstanding counts are never written, and a new primary starts them at zero
  ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).

> decision: an NFSv4.1 retry after its session was lost is a new request. The
> session — its slot table and replay cache — lives at the gateway node and dies
> with it: the client finds the session gone, creates a new one, and resends
> under a new slot and so a new request ID. A non-idempotent call whose reply was
> lost can then be applied twice, as after a restart of a server without
> persistent sessions, which NFS clients already tolerate; an exclusive create
> stays safe by its stored verifier
> ([RFC 7 §2.10](rfc-7-namespace-metadata.md#2.10%20Exclusive%20create)). NFSv3 and NFSv4.0 retries keep their identity
> across a gateway node loss, since it names no session. Overturned if re-applied
> non-idempotent calls across a gateway node loss show up in client-visible errors;
> then NFSv4.1 sessions are persisted in the store with their replay caches.

### 4.4 Node channels

Every message between nodes — a forwarded call, a replication message
([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)), a `vfs.Peers` call — travels on a
**node channel**:

- **Mutually authenticated.** Each node holds a credential the installation
  issues it, bound to its node ID, and kept as a sealed secret
  ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)). Both ends present theirs, and each **MUST** refuse a
  peer whose credential the installation did not issue, or whose node record is
  decommissioned.
- **Encrypted**, with integrity, so nothing on the network reads or alters a
  forwarded write or an offload notice.
- **Bound to the sender.** A receiver **MUST** refuse a message that names a
  node other than the channel's authenticated peer. Without that, any node — or
  anything that reaches the network — can forge an offload notice that makes a
  replica release dirty content, or a forwarded write under another node's name.

Collocated, a node channel is a function call and none of this applies.

## 5. Clients

### 5.1 pNFS

pNFS clients get layouts from a node's metadata-server endpoint,
which forwards `LAYOUTGET` to the primary of the file's shard, through a future
`Layouts` interface of the filesystem service
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)); the primary grants and
holds the layout as open state ([RFC 14](rfc-14-open-state.md)). Clients then send `READ` and `WRITE`
to the data servers the layout names.

**A layout names one data server: the primary of the file's shard.** A file is
never split across shards, so a layout never names more than one; striping one
file across data servers needs per-file or range shards, which are deferred to
[RFC 11 Appendix C](rfc-11-ownership.md#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards).

- **Fencing.** The layout records the (shard, epoch) it was granted under. The
  data server checks the layout's stateid and that (shard, epoch) on every
  `READ`, `WRITE` and `COMMIT`, so revoking one layout fences one client, and a
  data server that lost its shard refuses what it would once have accepted. A
  superseded data server **MUST NOT** answer a stable write or `COMMIT` without
  its replica set's durability ([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)).
- **Recall on primary change or move.** When the shard's primary changes, or the
  file moves to another shard, the layout **MUST** be recalled, and revoked at the
  recall deadline
  ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
- **Existence** is committed by the primary, fenced by the file's fence records,
  as on any other path: group-committed within a bounded age of the sync, with a
  stability point — a stable write or `COMMIT` — waiting only for the existence
  commit of an overwrite of committed content
  ([RFC 0 §5.1](rfc-0-data-lifecycle.md#5.1%20Write)). Appends and hole fills reach the store lazily, so
  the store does not reflect every flushed write until that commit lands. A `LAYOUTCOMMIT` for a
  layout whose epoch is stale **MUST** fail with `NFS4ERR_BADLAYOUT`, so the
  client rewrites through a fresh layout instead of committing a size over
  writes a failed primary lost.

> [!important] Release 1
> **A single node serves no pNFS.** It **MUST NOT** advertise itself as a pNFS
> metadata server, and layout operations — `GETDEVICEINFO`, `LAYOUTGET`,
> `LAYOUTCOMMIT`, `LAYOUTRETURN` — answer `NFS4ERR_NOTSUPP`. Layouts exist only
> under the rules above, which need a data server apart from the metadata
> server's own I/O path; [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) cites this rule.

### 5.2 SMB and NFSv3

These protocols have no data servers. Clients connect to any node, whose
gateway calls the local server when the node is the primary and forwards to the
primary otherwise: no extra hop when the client reached the primary's node, one
when it did not. pNFS clients use the same forwarding path
when a data server fails, as the NFSv4.1 specification requires, so it exists
in every topology.

### 5.3 Client addressing

Routing between nodes is invisible to clients, but a client holds a connection
to one node's address. When that node is lost or drained, something
**MUST** move the client, or it waits for its transport to time out, long after
any grace period has ended.

| Protocol | Mechanism | Level |
| --- | --- | --- |
| every protocol | **floating addresses.** Clients mount by addresses that belong to the cluster, not to a node. When a node is lost, a surviving one takes its addresses over, announces them on the network, and sends each client with a connection to the old node a TCP acknowledgement that makes it reset and reconnect at once | **MUST** |
| SMB | **the SMB Witness protocol.** A client registers for the share's address; the cluster notifies it of an address moving or a node draining, and tells it which node to move to | **MUST** on shares that offer continuous availability; **SHOULD** otherwise |
| NFSv4 | **filesystem locations** (`fs_locations`). A node that is drained answers with the locations of another node, so clients migrate before it stops | **MUST** for a planned drain |
| NFSv3 | floating addresses only | — |

**SMB continuous availability** is offered per share, only where persistent opens
are enabled ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)); it is off by default. A continuously available share
advertises it to clients and serves the Witness protocol; other shares offer
durable handles, which a client reconnecting after an address takeover
reclaims as long as the primary holding them did not change. A durable handle
does not survive the loss or restart of its primary; only a persistent one does
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).

A lost node is two losses at once. Its clients' connections are restored by an
address takeover and held no open state: the primaries hold it. The shards it was
primary of fail over, and their clients reclaim their state in them
([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)).

**A node is fenced before its addresses move.** Every node holds a node
lease ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)), and every call it forwards carries
its node epoch ([§4.3](#4.3%20The%20route%20envelope)). A surviving node **MUST NOT** take over a lost
node's addresses until, first, the lost node's lease has lapsed plus the drift
bound — measured as a duration on the marking node's monotonic clock from the
reply to a read of the lease's expiry record that still stands, never by comparing the
stored expiry with the marking node's `Now`
([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) —
and its node record is marked lapsed, and second, every
other node has acknowledged that mark — from then on refusing any call that
carries the fenced node epoch, and acknowledging only once every call it
admitted under that node epoch before the mark has ended, answered or refused —
or has itself lost its lease. Without the join, a call admitted just before the
mark could still apply after the acknowledgement. Without the rule, a
write the lost node forwarded before a partition can arrive after the client has
reconnected through the new address and written again, and land over the newer,
acknowledged write. A drain needs none of this: the draining node stops
forwarding before it releases its addresses.

**Marking a node lapsed waits out a store stall.** The mark that precedes an
address takeover is subject to the same claim hold as a shard takeover
([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)): a surviving node that could not reach
the store while the lost node's lease lapsed **MUST NOT** mark it until it has
reached the store again for the hold. Without it, after a store stall longer than
a lease the first node to reach the store marks every other one
lapsed, and a marked node cannot resume.

## 6. Learning primaries

A gateway routes by a cache of shard records read from the metadata store:

- **Correctness** is the receiver's: a call that reaches a former primary is refused
  by epoch ([§4.3](#4.3%20The%20route%20envelope)), and the caller re-reads and retries. A stale cache costs a
  refusal, never a wrong write.
- **Freshness** is a watch on the metadata store that pushes a shard's move, so
  refusals do not arrive in storms after a failover.
- A shard is **never encoded in a `FileID`** or a handle, not even as a hint: a
  file can change shards ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)), and the route is looked up from the
  file's recorded shard.

## 7. High availability needs no split

Surviving the loss of a node comes from four things, none of which needs a
second kind of node:

- the **metadata store** is replicated, so no node holds metadata nobody else can
  read;
- the **journal** is replicated ([RFC 10](rfc-10-journal-replication.md)), so an acknowledged write is on its
  replica set;
- **the primary fails over** ([RFC 11](rfc-11-ownership.md)) when its node lease lapses, at a higher
  epoch;
- **client addresses move** ([§5.3](#5.3%20Client%20addressing)).

A cluster of alike nodes over a replicated store is highly available. An
embedded single-node store gives none.

## 8. Invariants

| # | Invariant |
| --- | --- |
| T1 | Every node runs one binary and composes the same components, built by `node.New`; a node configured with one role refuses to start. |
| T2 | Each shard has one primary fenced by one epoch; no operation on one shard crosses between two primaries. |
| T3 | Every routed call carries a cluster-unique request ID, the shard and epoch it expects, and its sender's node epoch; a call under a stale epoch or a fenced node epoch is refused, and a retried mutation returns its first result from a dedup table keyed by request ID alone. |
| T4 | Every interface call is value-only and cursor-resumable, collocated or routed. |
| T5 | A stale route costs a refusal and a retry, never a wrong result. |
| T6 | A pNFS layout names one data server, the primary of the file's shard, is bound to its (shard, epoch), and is recalled when the shard changes primary or the file moves; `LAYOUTCOMMIT` on a stale layout fails with `NFS4ERR_BADLAYOUT`. |
| T7 | A client-facing address is taken over by a surviving node when its node is lost, and only after that node's lease has lapsed by the margins of [§5.3](#5.3%20Client%20addressing), the marking node has held the claim hold, and every other node refuses its node epoch and has ended every call it admitted under it. |
| T8 | A coordinator that is not the primary of a file it leaves without entries writes its pending release; only the file's primary releases it. |
| T9 | Every message between nodes travels on a mutually authenticated, encrypted channel whose authenticated peer is the node the message names. |
| T10 | An NFS request ID is the protocol's retry identity — (client address, XID, procedure, argument checksum) for NFSv3 and NFSv4.0, (gateway node's ID, the node epoch the session was created under, session, slot, sequence ID) for NFSv4.1 — so an NFSv3 or NFSv4.0 retry on a new connection or through another gateway node, and an NFSv4.1 retry on its session across a new node epoch of its gateway node, carries the same ID and is answered from a durable record of its first result, across a takeover of the primary too. A handover ships the open-state calls' entries with the open-state table. An SMB replay on another channel is matched at the primary by its create GUID, lock sequence or the channel sequence and outstanding counts in its `Open`, the channel sequence durable for a persistent open. |
| T11 | A single node advertises no pNFS and answers every layout operation with `NFS4ERR_NOTSUPP`. |
| T12 | A client call crosses between nodes only as a whole `vfs.Service` call, forwarded by `vfs.Gateway` and received by `vfs.Serve`. The gateway holds no file state and decides nothing: it makes no admission, identity, permission, ordering, open-state, metadata or engine call; only its router reads shard records and files' recorded shards. Every such decision is made by a `vfs.Server`: the primary's for a call naming a handle, the gateway node's own for one naming none. |

## 9. Conformance and benchmarks

- **Startup:** every configuration [§2.2](#2.2%20Startup%20refuses%20what%20cannot%20work) lists is refused.
- **Envelope:** drop the reply of a forwarded write, land a second write on the
  same extent, retry the first. Assert the second write's bytes survive (T3).
- **Stale layouts:** write through a layout, fail the shard's primary over, send
  `LAYOUTCOMMIT`. Assert `NFS4ERR_BADLAYOUT` and a size no larger than what the
  new primary holds (T6).
- **Release across shards:** open a file in one shard, rename over it from a
  directory in another. Assert the pending release is written and the content
  survives until the open closes (T8).
- **Address takeover:** kill a gateway node under NFSv3, NFSv4 and SMB load.
  Assert each client resumes within a bound far below its transport timeout (T7).
- **Late forwarded write:** partition a gateway node with a write in flight to
  a primary, let the client reconnect through the taken-over address and write
  the same extent again, then heal the partition. Assert the late write is
  refused by node epoch and the second write's bytes survive (T7). A takeover
  that does not fence first fails this.
- **The gateway decides nothing (cluster):** drive every `vfs.Service` method
  through a `vfs.Gateway` over a fake `vfs.Router` and `vfs.Transport`, with a
  local `vfs.Service` that records its calls. Assert each call reaches the local
  service or the transport unchanged, and that the gateway makes no other call
  (T12). A gateway that admits or squashes fails this.
- **Node channels:** send a replication message and a forwarded call from a peer
  without an issued credential, and from an authenticated peer naming another
  node. Assert both refused (T9).
- **Retry across a primary takeover:** over NFSv3, NFSv4.1 and SMB 3.x, drop the
  reply of a non-idempotent call, fail the primary over, let the client retry
  through the same gateway node. Assert one application and the first result. Repeat
  over NFSv4.0 and NFSv3 with the retry sent on a new connection, and through a
  second gateway node that took the client's address over: assert the same (T10). A
  request ID minted per connection applies the NFSv4.0 retry twice.
- **Request IDs at the edges (cluster):** a retry that finds its first send
  still running waits and returns its result; two gateway nodes that mint the same
  NFSv4.1 session ID each have their call applied; a gateway node that takes a new
  node epoch mid-session answers a retry of a call forwarded under the old one
  from its first result; an SMB `LOCK`, NFSv4.1 `CLOSE` and NFSv4.0 `LOCKU`
  whose replies are lost across a handover are each answered once (T3, T10;
  [RFC 11 §14](rfc-11-ownership.md#14.%20Test%20plan%20and%20benchmarks) runs them as scenarios).
- **Clone across shards (cluster):** a `CLONE` whose destination lies in
  another shard answers `NFS4ERR_XDEV`, a `COPY` of it is copied server-side
  and answers success with its count, and a later same-shard `CLONE` on the
  mount still clones; repeat with the two shards sharing a primary at
  admission ([§4.2](#4.2%20Calls%20that%20touch%20two%20primaries)). Then admit a
  slow same-shard clone and mark its directory per-child while it runs: assert
  the batch skips both files until the clone is done, then moves them.
- **Multichannel message IDs:** two channels of one SMB session send two
  different writes under the same message ID. Assert both are applied (T3, T10).
  A table keyed on (client GUID, session ID, message ID) answers the second with
  the first's result and fails this.
- **SMB replay on another channel:** lose the reply of a create and of a lock on
  one channel, replay each on a second channel. Assert one open and one lock;
  replay a write with a stale channel sequence and assert it is refused, and a
  read with a stale one and assert it is served; replay a write whose original
  is still in flight and assert `STATUS_FILE_NOT_AVAILABLE` at once, not a hold; then
  fail the primary over and replay the write again on a persistent open, and assert it
  is still refused (T10).
- **No pNFS on one node:** against a single node, assert `EXCHANGE_ID` does not
  advertise a pNFS metadata server and `LAYOUTGET` answers `NFS4ERR_NOTSUPP` (T11).
- **Marking after a stall:** stall the store for longer than a node lease
  (15 s at the 10 s default) under two nodes; assert neither marks the other lapsed, both resume, and no address
  moves (T7). A takeover with no claim hold fails this.

**Multi-node tests** run two nodes, each built by `node.New`, over an in-memory
control-plane store and a loopback `vfs.Transport`, and drive changes by writing
to the store:

- **Routing:** the filesystem service's conformance suite **MUST** pass
  unchanged through a gateway whose every call is forwarded, and with a stale
  route injected before every call.
- **Faults:** the loopback transport loses replies, sends twice and delivers
  under a stale epoch; assert each is answered once or refused (T3), and that a
  call a stale cache routes to the local path is refused as a forwarded one is. A nightly
  harness runs a cluster under partitions, pauses and node kills, and checks
  the history of client operations for linearizability per file.

**Benchmarks**, forwarded against local, same hardware: per-operation latency
added by one hop; SMB throughput through the primary's own node and through one
that forwards; pNFS bandwidth with the client's node and the data server
apart; time from a gateway node's loss to clients resuming; handover and
takeover time with a full retry window's dedup entries and request records at
the peak mutation rate.

## 10. Observability

Metric names are shown without the deployment's prefix.

| Answers | Metric | Type |
| --- | --- | --- |
| calls forwarded, by view and result `ok` or `stale_route` | `topology_forwarded_total{view, result}` | counter |
| refusals by epoch | `topology_epoch_refusals_total` | counter |
| retries answered from the dedup table | `topology_retries_deduplicated_total` | counter |
| hop latency | `topology_hop_seconds{view}` | histogram |
| primary-map pushes applied | `topology_primary_updates_total` | counter |
| client addresses taken over | `topology_address_takeovers_total` | counter |

Forwarding and epoch refusals are counted here only; other RFCs link to this
table.

## 11. Open questions

1. **Forwarding bounds.** A forwarding gateway streams (item 3); how its
   in-flight bytes are bounded is answered by RFC 24 §5.4: the payload is the
   request's receive buffer at the gateway node, and the primary charges a
   forwarded call to its own pools under the original client's key.
2. **Striping one file across data servers** waits on per-file and range shards
   ([RFC 11 Appendix C](rfc-11-ownership.md#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards)).
3. **The node-to-node wire format** is decided in outline: a typed RPC framework
   with schemas versioned for a rolling upgrade (gRPC is the example in mind),
   over the mutually authenticated channel of [§4.4](#4.4%20Node%20channels), with bidirectional
   streams carrying forwarded data and replication batches ([RFC 10](rfc-10-journal-replication.md)) so no
   hop buffers a whole payload. Open: the schemas themselves, and the rule for
   which versions a node accepts during an upgrade.

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| Scale-out NAS clusters that run protocol and storage on every node and forward each call to the node owning the file | one node type; forward to the owner rather than split the roles |
| Clusters whose protocol gateways are stateful clients writing straight to the storage daemons | why the roles stay together: such a split keeps open state in two places and needs a cluster-wide grace period |
| pNFS flex-files and multiprotocol NAS pNFS | per-client fencing by stateid; a layout segment per byte range with its own data server, for when range shards are promoted |
| A pNFS-native NAS exporting NFSv3 and SMB from its data nodes | the protocol role belongs with the data |
| Distributed file systems publishing epoch-numbered maps | primary caches corrected by epoch refusal and pushed updates |
| Clustered SMB servers with public-address takeover, and the SMB Witness protocol | floating addresses reset by acknowledgement; clients told where to move |
| Scale-out NAS clusters behind one DNS-delegated name, with dynamic address failover | clients spread across nodes by the cluster's own name server, and addresses moved on node loss as [§5.3](#5.3%20Client%20addressing) moves them; the contrast is lock state, which they keep in a lock manager spanning nodes and this design keeps at one primary per shard ([RFC 14 §3](rfc-14-open-state.md#3.%20One%20table%20per%20file%2C%20at%20one%20primary)) |
