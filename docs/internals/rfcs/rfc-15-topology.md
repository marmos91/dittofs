---
rfc: 15
title: "RFC 15 — topology and roles"
component: topology
status: draft
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

- define how a shard's primary is chosen, fenced, moved or failed over — [RFC 11](rfc-11-ownership.md);
- define journal replication — [RFC 10](rfc-10-journal-replication.md);
- define open state — [RFC 14](rfc-14-open-state.md). It says only which primary holds it;
- define a wire protocol between nodes. It states what every call must satisfy
  to cross one ([§4.1](#4.1%20Every%20call%20is%20safe%20to%20route)).

## 2. Roles

### 2.1 One binary, roles chosen at deployment

Every node runs the same binary. Which components it composes is set by its
**roles**, a setting fixed at start ([RFC 13](rfc-13-configuration.md)) and recorded on its node record
in the metadata store ([RFC 16](rfc-16-metadata-store.md)).

| Role | Composes | Needs |
| --- | --- | --- |
| **protocol** | NFS and SMB endpoints, the pNFS metadata-server endpoint, the filesystem service ([RFC 17](rfc-17-vfs.md)), the client-address agent ([§5.3](#5.3%20Client%20addressing)); holds no state of its own and forwards to primaries | the metadata store (reads), the primaries' addresses |
| **storage** | serving as primary or replica of shards ([RFC 11](rfc-11-ownership.md)) and, for the shards it is primary of, namespace writes, open state and locks ([RFC 14](rfc-14-open-state.md)), layouts, the content subsystem — journal and its replication ([RFC 10](rfc-10-journal-replication.md)), engine, carver, syncer, GC — and the pNFS data server; the management API | the metadata store, journal devices, remote-tier credentials |

The default is **both roles in one process**: a single node, where every call is
a function call and nothing in this document costs anything.

A split deployment runs `protocol`-only nodes in front of nodes with both roles,
or of `storage`-only nodes, against one replicated metadata store. A client that
reaches the node owning its file pays no hop; one that does not pays one
([§5.2](#5.2%20SMB%20and%20NFSv3)).

> ponytail: one storage role caps a shard's metadata operations and data
> bandwidth at one node together ([RFC 11 §2.1](rfc-11-ownership.md#2.1%20One%20primary%20per%20shard)). The upgrade is splitting
> `storage` into a `metadata` role and a `data` role, with a namespace primary and
> a data primary per shard. Take it on measured metadata/data contention on one
> primary node, or once separately scaled pNFS metadata and data servers are
> committed to.

### 2.2 Startup refuses what cannot work

A node **MUST** refuse to start when:

- it uses an embedded single-node metadata store and either is not the only node
  or does not run both roles;
- it runs `storage` without journal devices or remote-tier credentials.

A cluster **MUST** report itself unhealthy while it lacks a live node of each
role.

### 2.3 What a role does not hold

A node composes only what its roles need. A node without `storage` composes no
journal, engine, syncer or GC, holds no open state, and **MUST NOT** hold
remote-tier credentials: a compromised protocol node cannot reach the bucket.

### 2.4 Composition by role

One composition root, in the server's start command, builds a node from its
roles. No other component builds components; each is handed what it needs.

1. **Refuse** what [§2.2](#2.2%20Startup%20refuses%20what%20cannot%20work) lists, before anything opens.
2. **Metadata store**, embedded or a client of the replicated one
   ([RFC 16](rfc-16-metadata-store.md)).
3. With `storage`: **shard placement** ([RFC 11](rfc-11-ownership.md)), then the **content subsystem** —
   engine, journals and their replication, carver, syncer, GC — in the order
   [RFC 8 §2](rfc-8-engine.md#2.%20Composition) gives, then **namespace** and **open state** for the shards
   this node acquires, then the pNFS data server and the management API.
4. With `protocol`: the **router** ([§6](#6.%20Learning%20primaries)), the **filesystem service** over a local
   view of what this node owns and a remote view of everything else, the
   **client-address agent**, and last the **adapters**, which start accepting
   clients only when everything under them is serving.

Stop runs in reverse: adapters first, so no client call arrives at a component
already closed; a `storage` node then hands its shards over ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries))
before closing the content subsystem. [RFC 8 §2](rfc-8-engine.md#2.%20Composition) composes the content
subsystem only; this section is the root above it.

## 3. One primary per shard

[RFC 11](rfc-11-ownership.md) gives each shard **one primary**, a `storage` node named by the
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

Every interface of the metadata store ([RFC 16](rfc-16-metadata-store.md)) is served by one of these. The
filesystem service ([RFC 17](rfc-17-vfs.md)) routes each call; adapters never see the table.

| Served by | Calls |
| --- | --- |
| **any node** (store reads) | `Lookup`, `Entries`, `EntriesPlus`, `Files.Get` (the committed part), `ACL`, `Xattrs`, `Streams`, `Authorize`, `Resolve`, `Capacity.*`, `Principals.*` |
| **any `storage` node** (store transactions, no shard's primary) | `ControlPlane.*` |
| **the shard's primary** | `Create`, `Link`, `Unlink`, `Rename`, `SetAttrs`, `SetACL`, `SetXattr`, `RemoveXattr`, `OpenState.*`, `Existence.*`, the size and times overlay of `Files.Get`, reads and writes |
| **`storage` nodes, by sweep assignment** | `Blocks.*` ([RFC 9](rfc-9-gc.md)) |

A read **MAY** be served by any `storage` node under [RFC 11 §6](rfc-11-ownership.md#6.%20Reads%20on%20other%20nodes).

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
- **Prepare.** The coordinator asks the primary of every other shard involved to
  check the operation against its open state, recall the caching grants it
  breaks, and hold the files until an outcome or a deadline in store time. Each
  answers yes with its (shard, epoch), or no.
- **Commit.** On every yes, the coordinator commits **one** metadata-store
  transaction that guards the fence records of every file and directory it
  changes at the (shard, epoch) each primary answered with, and is refused if
  the store's commit time is past the earliest hold deadline. On any no, it
  aborts.
- **Release.** The coordinator sends the outcome, and each primary drops its hold.

**The coordinator does not decide a release it cannot see.** Whether a file with
no entries left may be released depends on its opens, which only the file's own
primary holds. A rename over a target, or an unlink, whose file is in a shard the
coordinator is not primary of **MUST** always write the file's pending release in its
transaction ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)), and the file's primary decides: it releases once no
open holds the file, and after grace if it is in one.

### 4.3 The route envelope

Every call a node forwards to a primary carries one envelope:

| Field | Means |
| --- | --- |
| request ID | unique per originating node and never reused by it; a retry reuses the original's |
| shard and epoch | the shard the sender routed by and the primary epoch it expects |
| hop count | zero from the front-end; a node that is not the primary refuses a call whose hop count is not zero rather than forward it ([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Front-ends%20forward%20to%20the%20primary)) |

- The primary **MUST** refuse a call whose epoch is not its current one for the
  shard ([§6](#6.%20Learning%20primaries)).
- The primary **MUST** keep a dedup table of recent mutations keyed by
  (shard, request ID) — not by epoch, so a retry that straddles an epoch raise
  is still recognised — holding each one's result for at least the sender's
  retry window, and **MUST** answer a retry from it rather than apply it again.
  The table is handed over with every handover and with the files of every
  batch move ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)). A
  write whose reply was lost, retried after another write landed on the same
  extent, then returns its first result instead of overwriting the second.
- A journal operation carries its request ID into the journal and to every
  replica ([RFC 10 §4](rfc-10-journal-replication.md#4.%20The%20write%20path)), so a new primary answers a retry of an operation it holds
  from its journal, not by applying it again.

> ponytail: the dedup table is memory at the primary, so a namespace mutation
> whose reply was lost across a failover is applied again on retry, and a
> non-idempotent one answers as it would after any server restart (an exclusive
> create finds the file it made). Upgrade by recording the request ID in the
> mutation's transaction when that outcome shows up in client-visible errors.

## 5. Clients

### 5.1 pNFS

pNFS clients get layouts from the `protocol` node's metadata-server endpoint,
which forwards `LAYOUTGET` to the primary of the file's shard; the primary grants and
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
- **Existence** is committed by the primary at its stability point — a stable
  write or `COMMIT` — fenced by the file's fence records. A `LAYOUTCOMMIT` for a
  layout whose epoch is stale **MUST** fail with `NFS4ERR_BADLAYOUT`, so the
  client rewrites through a fresh layout instead of committing a size over
  writes a failed primary lost.

### 5.2 SMB and NFSv3

These protocols have no data servers. Clients connect to a `protocol` node,
which serves reads and writes against the primary beside it, or forwards to it:
no extra hop when the client reached the primary's node, one when it did not. pNFS clients use the same forwarding path
when a data server fails, as the NFSv4.1 specification requires, so it exists
in every topology.

### 5.3 Client addressing

Routing between nodes is invisible to clients, but a client holds a connection
to one `protocol` node's address. When that node is lost or drained, something
**MUST** move the client, or it waits for its transport to time out, long after
any grace period has ended.

| Protocol | Mechanism | Level |
| --- | --- | --- |
| every protocol | **floating addresses.** Clients mount by addresses that belong to the cluster, not to a node. When a `protocol` node is lost, a surviving one takes its addresses over, announces them on the network, and sends each client with a connection to the old node a TCP acknowledgement that makes it reset and reconnect at once | **MUST** |
| SMB | **the SMB Witness protocol.** A client registers for the share's address; the cluster notifies it of an address moving or a node draining, and tells it which node to move to | **MUST** on shares that offer continuous availability; **SHOULD** otherwise |
| NFSv4 | **filesystem locations** (`fs_locations`). A node that is drained answers with the locations of another `protocol` node, so clients migrate before it stops | **MUST** for a planned drain |
| NFSv3 | floating addresses only | — |

**SMB continuous availability** is offered per share, only where persistent opens
are enabled ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)); it is off by default. A continuously available share
advertises it to clients and serves the Witness protocol; other shares offer
durable handles, which a client reconnecting after an address takeover
reclaims.

A lost `protocol` node loses no open state: the primaries hold it. A lost primary is
a failover, and its clients reclaim their state in its shards
([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)); with both roles on one node, both happen at once.

## 6. Learning primaries

A `protocol` node routes by a cache of shard records read from the metadata store:

- **Correctness** is the receiver's: a call that reaches a former primary is refused
  by epoch ([§4.3](#4.3%20The%20route%20envelope)), and the caller re-reads and retries. A stale cache costs a
  refusal, never a wrong write.
- **Freshness** is a watch on the metadata store that pushes a shard's move, so
  refusals do not arrive in storms after a failover.
- A shard is **never encoded in a `FileID`** or a handle: a file can change shards
  ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)). It **MAY** be carried as a hint.

## 7. High availability needs no split

Surviving the loss of a node comes from four things, none of which needs roles
to be split:

- the **metadata store** is replicated, so no node holds metadata nobody else can
  read;
- the **journal** is replicated ([RFC 10](rfc-10-journal-replication.md)), so an acknowledged write is on its
  replica set;
- **the primary fails over** ([RFC 11](rfc-11-ownership.md)) when its node lease lapses, at a higher
  epoch;
- **client addresses move** ([§5.3](#5.3%20Client%20addressing)).

A cluster of nodes with both roles is highly available. Splitting `protocol`
from `storage` buys isolation — no remote-tier credentials where clients connect
— and scales client connections apart from storage. An embedded single-node
store gives no high availability, whatever the roles.

## 8. Invariants

| # | Invariant |
| --- | --- |
| T1 | Every node runs one binary; its roles decide what it composes, one composition root builds it, and it holds no credentials a role of its does not need. |
| T2 | Each shard has one primary fenced by one epoch; no operation on one shard crosses between two primaries. |
| T3 | Every routed call carries a request ID and the shard and epoch it expects; a call under a stale epoch is refused, and a retried mutation returns its first result from a dedup table keyed by (shard, request ID). |
| T4 | Every interface call is value-only and cursor-resumable, collocated or routed. |
| T5 | A stale route costs a refusal and a retry, never a wrong result. |
| T6 | A pNFS layout names one data server, the primary of the file's shard, is bound to its (shard, epoch), and is recalled when the shard changes primary or the file moves; `LAYOUTCOMMIT` on a stale layout fails with `NFS4ERR_BADLAYOUT`. |
| T7 | A client-facing address is taken over by a surviving `protocol` node when its node is lost. |
| T8 | A coordinator that is not the primary of a file it leaves without entries writes its pending release; only the file's primary releases it. |

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
- **Address takeover:** kill a `protocol` node under NFSv3, NFSv4 and SMB load.
  Assert each client resumes within a bound far below its transport timeout (T7).

**Split-mode tests are stated here once**, and run from the first release that
ships a remote view; until then there is nothing remote to test:

- **Routing:** the metadata store's and the filesystem service's conformance
  suites **MUST** pass unchanged with every view remote, and with a stale route
  injected before every call.
- **Faults:** a nightly harness runs a split cluster under partitions, pauses and
  node kills, and checks the history of client operations for linearizability
  per file.

**Benchmarks**, split against collocated, same hardware: per-operation latency
added by one hop; SMB throughput through a `protocol` node on the primary's node
and one that forwards; pNFS bandwidth with the client's node and the data server
apart; time from a `protocol` node's loss to clients resuming.

## 10. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| calls forwarded, by view and result `ok` or `stale_route` | `dittofs_topology_forwarded_total{view, result}` | counter |
| refusals by epoch | `dittofs_topology_epoch_refusals_total` | counter |
| retries answered from the dedup table | `dittofs_topology_retries_deduplicated_total` | counter |
| hop latency | `dittofs_topology_hop_seconds{view}` | histogram |
| primary-map pushes applied | `dittofs_topology_primary_updates_total` | counter |
| client addresses taken over | `dittofs_topology_address_takeovers_total` | counter |
| roles of this node | `dittofs_topology_roles{role}` | gauge |

Forwarding and epoch refusals are counted here only; other RFCs link to this
table.

## 11. Open questions

1. **Forwarding bounds.** Whether a forwarding `protocol` node streams or buffers,
   and how that is bounded ([RFC 24](rfc-index.md)).
2. **Striping one file across data servers** waits on per-file and range shards
   ([RFC 11 Appendix C](rfc-11-ownership.md#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards)).

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| Parallel file systems with separate metadata and object servers on shared hosts | roles as deployment, not code |
| pNFS flex-files and multiprotocol NAS pNFS | per-client fencing by stateid; a layout segment per byte range with its own data server, for when range shards are promoted |
| A pNFS-native NAS exporting NFSv3 and SMB from its data nodes | the protocol role belongs with the data |
| Distributed file systems publishing epoch-numbered maps | primary caches corrected by epoch refusal and pushed updates |
| Clustered SMB servers with public-address takeover, and the SMB Witness protocol | floating addresses reset by acknowledgement; clients told where to move |
