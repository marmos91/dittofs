---
rfc: 15
title: "RFC 15 — topology and roles"
component: topology
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
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

**Status:** draft. [§11](#11.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone deploying more than one node, adding a component that must
run on a particular kind of node, or deciding where a call is served.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md).

---

## 1. Purpose

One node runs everything. Several nodes need to know which of them run what,
which one owns each file, and where each call goes. This document answers:

> **What does each node run, who owns each file, and which node serves each
> call?**

### 1.1 Non-goals

This document **MUST NOT**:

- define how ownership is acquired, fenced, moved or failed over — [RFC 11](rfc-11-ownership.md);
- define journal replication — [RFC 10](rfc-10-journal-replication.md);
- define open state — [RFC 14](rfc-14-open-state.md). It says only which owner holds it;
- define a wire protocol between nodes. It states what every call must satisfy
  to cross one ([§4.1](#4.1%20Every%20call%20is%20safe%20to%20route)).

## 2. Roles

### 2.1 One binary, roles chosen at deployment

Every node runs the same binary. Which components it composes is set by its
**roles**, a setting fixed at start ([RFC 13](rfc-13-configuration.md)) and recorded on its node record
in the metadata store ([RFC 16](rfc-16-metadata-store.md)).

| Role | Composes | Needs |
| --- | --- | --- |
| **protocol** | NFS and SMB endpoints, the pNFS metadata-server endpoint, the filesystem service ([RFC 17](rfc-17-vfs.md)); holds no state of its own and forwards to owners | the metadata store (reads), the owners' addresses |
| **metadata** | namespace ownership, open state and locks ([RFC 14](rfc-14-open-state.md)), layouts, the management API | the metadata store |
| **data** | the journal and its replication ([RFC 10](rfc-10-journal-replication.md)), carver, syncer, offload, GC, the pNFS data server, data ownership | the metadata store, journal devices, remote-tier credentials |

The default is **all three in one process**: a single node, where every call is
a function call and nothing in this document costs anything.

A split deployment runs nodes with fewer roles against one replicated metadata
store. The usual split is **`protocol`+`data` nodes and `metadata` nodes**: the
protocol role is where clients connect, and it belongs next to the data, or
every SMB and NFSv3 byte crosses the metadata tier.

### 2.2 Startup refuses what cannot work

A node **MUST** refuse to start when:

- it uses an embedded single-node metadata store and either is not the only node
  or does not run all three roles;
- it runs `data` without journal devices or remote-tier credentials.

A cluster **MUST** report itself unhealthy while it lacks a live node of each
role.

### 2.3 What a role does not hold

A node composes only what its roles need. A node without `data` composes no
journal, syncer or GC, and **MUST NOT** hold remote-tier credentials: a
compromised protocol or metadata node cannot reach the bucket. A node without
`metadata` holds no open state.

> [!important] Pending review — roles
> New. Three roles in one binary, all three by default; `protocol` placed with
> `data` in a split. A node holds only the credentials its roles need.

## 3. Two owners per file

[RFC 11](rfc-11-ownership.md) gives each ownership unit an owner. With roles, a unit has **two
owners**, each acquired, leased and fenced by its own epoch under RFC 11's rules:

| Owner | Held by | Serialises |
| --- | --- | --- |
| **namespace owner** | a `metadata` node | namespace writes, open state and locks, caching grants, layouts, pending releases |
| **data owner** | a `data` node | journal appends, existence commits, offload, removals, clone |

On a node running both roles, one node holds both and nothing crosses the
network.

### 3.1 Where the two owners meet

- **Release.** The namespace owner decides it — the last entry and the last open
  are gone ([RFC 7 §4](rfc-7-namespace-metadata.md#4.%20What%20keeps%20a%20file%20alive)) — and the data owner executes it — drop the journal's
  copy and run the removal. The pending-release record is the handoff: written by
  the first in the transaction that removes the last holder, consumed by the
  second, so a crash of either resumes it.
- **Truncate and an explicit size.** The namespace owner authorises them; the
  data owner applies them in its journal order, as existence.
- **`size` and write-time times.** They belong to the data owner until committed
  ([RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives)). A `GETATTR` reads the committed file from the metadata store
  and, only while the data owner holds uncommitted writes for that file, asks it
  for the overlay.
- **`LAYOUTCOMMIT`.** It arrives at the namespace owner, which **MUST** ask the
  data owner to commit the reported size and times as existence, and answer only
  after that commit.

> [!important] Pending review — two owners per file
> RFC 11 had one owner per unit. Namespace and data ownership now have separate
> epochs; release, truncate, the size overlay and `LAYOUTCOMMIT` are the four
> handoffs between them.

## 4. Where each call runs

Every interface of the metadata store ([RFC 16](rfc-16-metadata-store.md)) is served by one of these. The
filesystem service ([RFC 17](rfc-17-vfs.md)) routes each call; adapters never see the table.

| Served by | Calls |
| --- | --- |
| **any node** (store reads) | `Lookup`, `Entries`, `EntriesPlus`, `Files.Get` (the committed part), `ACL`, `Xattrs`, `Streams`, `Authorize`, `Resolve`, `Capacity.*`, `Principals.*` |
| **any `metadata` node** (store transactions, no file owner) | `ControlPlane.*` |
| **namespace owner** | `Create`, `Link`, `Unlink`, `Rename`, `SetAttrs` except size, `SetACL`, `SetXattr`, `RemoveXattr`, `OpenState.*` |
| **data owner** | `SetAttrs` size, `Existence.*`, `Content.*`, the overlay half of `Files.Get`, reads and writes |
| **`data` nodes, by sweep assignment** | `Blocks.*` ([RFC 9](rfc-9-gc.md)) |

A read **MAY** be served by any `data` node under [RFC 11 §6](rfc-11-ownership.md#6.%20Reads%20on%20non-owners).

### 4.1 Every call is safe to route

For a call to cross a network unchanged, every interface method **MUST**:

- take and return values, never shared pointers or handles to store state;
- resume an iteration from a cursor the caller holds, never from iterator state
  kept by the server;
- be idempotent, or retried inside the call, so a retry after a lost reply cannot
  apply twice;
- carry the owner epoch it expects, so a call reaching a former owner is refused
  rather than applied.

Collocated, the same interface is a function call; the caller cannot tell which
it has.

### 4.2 Calls that touch two owners

A rename across two namespace units, and a create whose new file lands in a
different unit from its directory, touch two namespace owners. Both are single
metadata-store transactions that conflict correctly whoever runs them. The
namespace owner of the **source directory** coordinates, and **MUST** carry both
units' epochs.

## 5. Clients

### 5.1 pNFS

pNFS clients get layouts from the namespace owner and send `READ` and `WRITE`
to `data` nodes directly. A layout names **one data server per file**: the data
owner. `LAYOUTCOMMIT` is the existence commit ([§3.1](#3.1%20Where%20the%20two%20owners%20meet)). The data owner checks
the layout's stateid and its own epoch, so revoking one layout fences one client.

**Striping one file across data servers is out of scope.** It means several
writers for one file, which one data owner per unit and per-file fences exclude.
Parallelism comes from spreading files across data nodes, which needs units
finer than a share ([RFC 11 §2](rfc-11-ownership.md#2.%20Ownership%20units)). Per-range data ownership is the upgrade if
single-file bandwidth is shown to matter.

### 5.2 SMB and NFSv3

These protocols have no data servers. Clients connect to a `protocol` node,
which serves reads and writes against the data owner beside it, or forwards to
it: no extra hop when the client reached the owner's node, one when it did not.
pNFS clients use the same forwarding path when a data server fails, as the
NFSv4.1 specification requires, so it exists in every topology.

## 6. Learning owners

A `protocol` node routes by a cache of ownership units read from the metadata
store:

- **Correctness** is the receiver's: a call that reaches a former owner is refused
  by epoch ([§4.1](#4.1%20Every%20call%20is%20safe%20to%20route)), and the caller re-reads and retries. A stale cache costs a
  refusal, never a wrong write.
- **Freshness** is a watch on the metadata store that pushes a unit's move, so
  refusals do not arrive in storms after a failover.
- A unit is **never encoded in a `FileID`** or a handle: a file can change units
  ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20ownership)). It **MAY** be carried as a hint.

## 7. High availability needs no split

Surviving the loss of a node comes from three things, none of which needs roles
to be split:

- the **metadata store** is replicated, so no node holds metadata nobody else can
  read;
- the **journal** is replicated ([RFC 10](rfc-10-journal-replication.md)), so an acknowledged write is on its
  replica set;
- **ownership fails over** ([RFC 11](rfc-11-ownership.md)) when an owner's lease lapses, at a higher
  epoch.

A cluster of all-role nodes is highly available. Splitting roles buys isolation
and independent scaling — metadata operations and data bandwidth grow on
different axes — and is what pNFS's metadata and data servers need. An embedded
single-node store gives no high availability, whatever the roles.

## 8. Invariants

| # | Invariant |
| --- | --- |
| T1 | Every node runs one binary; its roles decide what it composes, and it holds no credentials a role of its does not need. |
| T2 | Each unit has one namespace owner and one data owner, each fenced by its own epoch. |
| T3 | Release, truncate, the size overlay and `LAYOUTCOMMIT` cross owners only through the handoffs of §3.1, and a crash of either side resumes them. |
| T4 | Every interface call is value-only, cursor-resumable, retry-safe and epoch-carrying, collocated or routed. |
| T5 | A stale route costs a refusal and a retry, never a wrong result. |
| T6 | A pNFS layout names one data server per file, and revoking it fences one client. |

## 9. Conformance and benchmarks

- **Routing:** the metadata store's and the filesystem service's conformance
  suites **MUST** pass unchanged with every view remote, and with a stale route
  injected before every call.
- **Handoffs:** kill either owner at every step of each §3.1 handoff; assert it
  resumes and applies once.
- **Startup:** every configuration §2.2 lists is refused.
- **Benchmarks**, split against collocated, same hardware: per-operation latency
  added by one hop; `GETATTR` with and without an overlay; SMB throughput through
  a `protocol`+`data` node that owns the file, and one that forwards.

## 10. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| calls forwarded, by view and target role | `dittofs_topology_forwarded_total{view, role}` | counter |
| refusals by epoch, by owner kind | `dittofs_topology_epoch_refusals_total{owner=namespace\|data}` | counter |
| hop latency | `dittofs_topology_hop_seconds{view}` | histogram |
| owner-map pushes applied | `dittofs_topology_owner_updates_total` | counter |
| roles of this node | `dittofs_topology_roles{role}` | gauge |

## 11. Open questions

1. **Unit granularity for pNFS shares.** Per-file units multiply ownership records
   and leases by the file count.
2. **Forwarding bounds.** Whether a forwarding `protocol` node streams or buffers,
   and how that is bounded ([RFC 24](rfc-index.md)).
3. **Placing namespace and data owners together.** Whether a unit's two owners
   **SHOULD** prefer one node when both roles are available there.

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| Parallel file systems with separate metadata and object servers on shared hosts | roles as deployment, not code; metadata and data scaled apart |
| pNFS flex-files and multiprotocol NAS pNFS | one data server per file is viable; per-client fencing by stateid |
| A pNFS-native NAS exporting NFSv3 and SMB from its data nodes | the protocol role belongs with data |
| Distributed file systems publishing epoch-numbered maps | owner caches corrected by epoch refusal and pushed updates |
