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
**Audience:** anyone deploying more than one node, adding a component that must
run on a particular kind of node, or deciding where a call is served.

> [!note] Built after the single-node release
> This design is not shelved. The first release is a single node scaled
> vertically; horizontal scaling is the phase after it, needed for large
> contracts, for large shares spread over several nodes and, later, for
> NFSv4.2 and pNFS. Every rule here is a cluster rule; which of them bind the
> first release, and what replaces the rest on one node, is
> [RFC 0's single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile).
> Until the cluster is built only these hooks are implemented:
>
> - the 128-bit content version, with its epoch half held at zero
>   ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions));
> - the node epoch and the shard incarnation carried in the NFS write verifier
>   ([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state), rule 3);
> - one binary, with roles chosen by configuration ([§2.1](#2.1%20One%20binary%2C%20roles%20chosen%20at%20deployment)).
>
> Adding nodes migrates each journal's format one way, behind a gate
> ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)).

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md).

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

One node can run all of DittoFS. Several nodes need to agree on three things:
what each node runs, which node is in charge of each file, and which node a
client's call goes to. This RFC answers them. It gives every node one or both
of two **roles** — `protocol`, which talks to clients, and `storage`, which
holds the data — and says how calls travel between them and how clients follow
when a node is lost.

In outline: clients reach DittoFS over NFS (Network File System) or SMB (Server
Message Block); writes land in a local journal and are later uploaded to an
S3 (Simple Storage Service) bucket; a metadata store records where everything
is ([RFC 0](rfc-0-data-lifecycle.md)). The first release is one node with both
roles, where everything here is a function call and costs nothing; this is the
design for adding nodes after it.

### The problem, in one example

The installation runs as a cluster: protocol nodes P1 and P2, storage nodes S1,
S2 and S3, one replicated metadata store. Files are grouped into **shards**,
each with one **primary**: shard A, `profiles/alice/`, has primary S1; shard B,
`profiles/bob/`, has primary S2. alice-pc maps `\\10.0.0.50\profiles`, an
address that belongs to the cluster and is held, for now, by P1.

1. **A write.** alice-pc writes 64 KiB into `ODFC_alice.vhdx`. P1 holds no
   state of its own: it looks the file's shard up in its cached copy of the
   shard records, and forwards the write to S1 with an **envelope** — a request
   ID unique in the cluster, the shard and the epoch P1 expects, and a hop
   count of zero. S1 checks the epoch, applies the write and replies.
2. **A lost reply.** The reply is lost and P1 sends the write again under the
   same request ID. Meanwhile another write has landed on the same bytes.
   Applying the retry would overwrite it; S1 instead answers from its table of
   recent results, keyed by request ID.
3. **P1 dies.** Without help, alice-pc would wait for its connection to time
   out, long after the window in which it could reclaim its opens. Instead,
   once P1's node lease has lapsed and every storage node has fenced P1, so no
   call P1 still has in flight can land, P2
   takes over `10.0.0.50`, announces it on the network, and sends alice-pc a
   TCP acknowledgement that makes it reset and reconnect at once. On a share
   offering continuous availability, the SMB Witness protocol also tells
   alice-pc that the address moved and where to go. alice-pc reconnects to P2
   and finds its durable open, which S1 held all along: losing a protocol node
   loses no open state.
4. **S1 dies.** A storage node is lost instead. Shard A fails over to another
   storage node at a higher epoch ([RFC 11](rfc-11-ownership.md)), and its
   clients reclaim their opens in a grace period
   ([RFC 14](rfc-14-open-state.md)). P2's next call still names S1's epoch; it
   is refused, P2 re-reads the shard record and retries. A stale route costs
   one refusal, never a write applied by a node that is no longer in charge.

```text
                      alice-pc (SMB)  \\10.0.0.50\profiles
                          │
           10.0.0.50 held by P1 ── P1 lost ──► P2 takes 10.0.0.50,
                          │                    resets alice-pc's TCP,
                          │                    Witness says where to go
                          ▼
  ┌ protocol: P1, P2 ─────────────────────────────────────────────┐
  │ adapters, filesystem service, cached shard records; no state, │
  │ no bucket credentials                                         │
  └───────┬───────────────────────────────────────────────────────┘
          │ envelope: request P1#4711, shard A, epoch 7, hop 0
          ▼
  ┌ storage: S1 ─────────────┐ ┌ S2 ─────────────┐ ┌ S3 ──────────┐
  │ primary of shard A       │ │ primary of B    │ │ replica of A │
  │ profiles/alice/: open    │ │ profiles/bob/   │ │              │
  │ state, journal, writes,  │ │                 │ │              │
  │ results by request ID    │ │                 │ │              │
  └──────────────────────────┘ └─────────────────┘ └──────────────┘
          shard records: A ─► S1 at epoch 7 (replicated metadata store)
```

The whole cluster, with every role and what it shares:

![A cluster: protocol nodes P1 and P2 behind floating addresses forward each call to the primary of the file's shard; storage nodes S1, S2, S3 are each primary of one shard and replica of the others, shipping journal records between them; all share a replicated metadata store and one S3 bucket](img/rfc0-cluster.svg)

### The words you need

- **[Node](rfc-0-data-lifecycle.md#Glossary)** and
  **[role](rfc-0-data-lifecycle.md#Glossary)**: one DittoFS server process;
  `protocol` runs the adapters and the filesystem service, `storage` runs the
  journals, open state and content path. Both, by default
  ([§2.1](#2.1%20One%20binary%2C%20roles%20chosen%20at%20deployment)).
- **[Shard](rfc-0-data-lifecycle.md#Glossary)** and
  **[primary](rfc-0-data-lifecycle.md#Glossary)**: a set of files, a share by
  default, and the one storage node that accepts its writes at a time
  ([§3](#3.%20One%20primary%20per%20shard)).
- **[Epoch](rfc-0-data-lifecycle.md#Glossary)**: a number in a shard's record,
  raised by every change that must fence a sender — a new primary, a replica
  change, the start of a move — but not by a lone node's re-claim of its own
  shard ([RFC 10 §10](rfc-10-journal-replication.md#10.%20A%20single%20node)); a call carrying an older one is
  refused.
- **Route envelope**: what every forwarded call carries: request ID, shard and
  epoch, sending node and node epoch, hop count ([§4.3](#4.3%20The%20route%20envelope)).
- **Floating address**: a client-facing address owned by the cluster, taken
  over by a surviving protocol node ([§5.3](#5.3%20Client%20addressing)).
- **SMB Witness**: the SMB protocol by which a cluster tells a client that an
  address moved or a node is draining, and where to go
  ([§5.3](#5.3%20Client%20addressing)).

### What this RFC promises

- Every node runs the same binary; its roles decide what it builds, and a node
  without `storage` holds no bucket credentials.
- Each shard has one primary at a time. Every change to its files, their open
  state and their writes runs there, so a conflict check and the I/O it admits
  are never on two nodes.
- A call that reaches a node no longer in charge is refused, never applied; a
  retried change returns its first result instead of applying twice.
- When a protocol node is lost, a survivor takes its client addresses and
  makes its clients reconnect at once.
- A cluster of nodes with both roles is highly available; splitting the roles
  buys isolation and separate scaling, not availability.

### How the rest is organised

[§2](#2.%20Roles) defines the roles, what each composes, and the start-up
checks. [§3](#3.%20One%20primary%20per%20shard) is the one-primary rule.
[§4](#4.%20Where%20each%20call%20runs) says which node serves each call, what
makes a call safe to route, how calls spanning two shards run, and the
envelope. [§5](#5.%20Clients) covers clients: pNFS (parallel NFS), SMB and
NFSv3, and client addressing. [§6](#6.%20Learning%20primaries) is how protocol
nodes learn primaries, [§7](#7.%20High%20availability%20needs%20no%20split)
why availability needs no split, and [§8](#8.%20Invariants) the invariants. On
a first read, skip §2.4, §4.2 and §5.1.

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
  breaks, and hold the files until an outcome or a deadline in store time, no
  later than its own node lease expiry less the drift bound, and first commit a
  durable hold record for the operation. Each answers yes with its (shard,
  epoch) and (node, node epoch), or no.
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

### 4.3 The route envelope

Every call a node forwards to a primary carries one envelope:

| Field | Means |
| --- | --- |
| request ID | unique across the cluster, and the same on every retry of one client request. Where the client-facing protocol names a retry itself, the ID is derived from that name, so a retry the client sends through another front-end carries the same ID: NFSv4.1 and later, the client ID, session ID, slot and sequence ID; SMB 3.x, the session's client GUID, the session ID and the message ID of a request flagged as a replay, and for a create the create GUID. Where it names none — NFSv3, NFSv4.0 — the originating node's ID and a number that node never reuses, and a retry through the same node reuses the original's |
| shard and epoch | the shard the sender routed by and the primary epoch it expects |
| node and node epoch | the forwarding node and the node epoch of its lease; a primary refuses a call from a node epoch that has been fenced ([§5.3](#5.3%20Client%20addressing)) |
| hop count | zero from the front-end; a node that is not the primary refuses a call whose hop count is not zero rather than forward it ([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Front-ends%20forward%20to%20the%20primary)) |

- The primary **MUST** refuse a call whose epoch is not its current one for the
  shard ([§6](#6.%20Learning%20primaries)), and one whose sending node epoch it has fenced.
- The primary **MUST** keep a dedup table of recent mutations keyed by the
  request ID alone — not by shard or epoch, so a retry that straddles an epoch
  raise, or is re-routed to the shard its file moved to, is still recognised —
  holding each one's result for at least the sender's
  retry window, and **MUST** answer a retry from it rather than apply it again.
  The table is handed over with every handover and with the files of every
  batch move ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)). A
  write whose reply was lost, retried after another write landed on the same
  extent, then returns its first result instead of overwriting the second.
- **The table survives a takeover (cluster).** A journal operation carries its
  request ID into the journal and to every replica, and its record keeps the
  request ID and result ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)); a namespace mutation writes a
  request record with its result in its own transaction
  ([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Front-ends%20forward%20to%20the%20primary)). A new primary rebuilds the table from both before it
  serves, so a retry after a failover is answered, not applied again.

> ponytail: a request ID derived from a client's own session survives a change of
> front-end, but NFSv3 and NFSv4.0 name no retry, so their retries through a
> different front-end after an address takeover get a new ID and are applied as
> new requests — as after any server restart, which those clients already
> tolerate; an exclusive create stays safe by its stored verifier
> ([RFC 7 §2.10](rfc-7-namespace-metadata.md#2.10%20Exclusive%20create)). Upgrade by keying those protocols' entries on
> (client address, transaction ID) as a duplicate-request cache does, if
> retried non-idempotent calls across a takeover show up in client-visible errors.

### 4.4 Node channels

Every message between nodes — a forwarded call, a replication message
([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)), a prepare of a cross-shard operation — travels on a
**node channel**:

- **Mutually authenticated.** Each node holds a credential the installation
  issues it, bound to its node ID, and kept as a sealed secret
  ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)). Both ends present theirs, and each **MUST** refuse a
  peer whose credential the installation did not issue, or whose node record is
  decommissioned.
- **Encrypted**, with integrity, so nothing on the network reads or alters a
  forwarded write or a durability notice.
- **Bound to the sender.** A receiver **MUST** refuse a message that names a
  node other than the channel's authenticated peer. Without that, any node — or
  anything that reaches the network — can forge a durability notice that makes a
  replica release dirty content, or a forwarded write under another node's name.

Collocated, a node channel is a function call and none of this applies.

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
reclaims as long as the primary holding them did not change. A durable handle
does not survive the loss or restart of its primary; only a persistent one does
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).

A lost `protocol` node loses no open state: the primaries hold it. A lost primary is
a failover, and its clients reclaim their state in its shards
([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)); with both roles on one node, both happen at once.

**A node is fenced before its addresses move.** A `protocol` node holds a node
lease like a `storage` node ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)), and every call it forwards carries
its node epoch ([§4.3](#4.3%20The%20route%20envelope)). A surviving node **MUST NOT** take over a lost
node's addresses until, first, the lost node's lease has lapsed plus the drift
bound in store time and its node record is marked lapsed, and second, every
`storage` node has acknowledged that mark — from then on refusing any call that
carries the fenced node epoch — or has itself lost its lease. Without it, a
write the lost node forwarded before a partition can arrive after the client has
reconnected through the new address and written again, and land over the newer,
acknowledged write. A drain needs none of this: the draining node stops
forwarding before it releases its addresses.

## 6. Learning primaries

A `protocol` node routes by a cache of shard records read from the metadata store:

- **Correctness** is the receiver's: a call that reaches a former primary is refused
  by epoch ([§4.3](#4.3%20The%20route%20envelope)), and the caller re-reads and retries. A stale cache costs a
  refusal, never a wrong write.
- **Freshness** is a watch on the metadata store that pushes a shard's move, so
  refusals do not arrive in storms after a failover.
- A shard is **never encoded in a `FileID`** or a handle, not even as a hint: a
  file can change shards ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)), and the route is looked up from the
  file's recorded shard.

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
| T3 | Every routed call carries a cluster-unique request ID, the shard and epoch it expects, and its sender's node epoch; a call under a stale epoch or a fenced node epoch is refused, and a retried mutation returns its first result from a dedup table keyed by request ID alone. |
| T4 | Every interface call is value-only and cursor-resumable, collocated or routed. |
| T5 | A stale route costs a refusal and a retry, never a wrong result. |
| T6 | A pNFS layout names one data server, the primary of the file's shard, is bound to its (shard, epoch), and is recalled when the shard changes primary or the file moves; `LAYOUTCOMMIT` on a stale layout fails with `NFS4ERR_BADLAYOUT`. |
| T7 | A client-facing address is taken over by a surviving `protocol` node when its node is lost, and only after that node's lease has lapsed and every `storage` node refuses its node epoch. |
| T8 | A coordinator that is not the primary of a file it leaves without entries writes its pending release; only the file's primary releases it. |
| T9 | Every message between nodes travels on a mutually authenticated, encrypted channel whose authenticated peer is the node the message names. |
| T10 | A retried request carries the request ID its client-facing session names where the protocol names one, and is answered from a durable record of its first result across a takeover. |

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
- **Late forwarded write:** partition a `protocol` node with a write in flight to
  a primary, let the client reconnect through the taken-over address and write
  the same extent again, then heal the partition. Assert the late write is
  refused by node epoch and the second write's bytes survive (T7). A takeover
  that does not fence first fails this.
- **Node channels:** send a replication message and a forwarded call from a peer
  without an issued credential, and from an authenticated peer naming another
  node. Assert both refused (T9).
- **Retry through another front-end:** over NFSv4.1 and SMB 3.x, drop the reply
  of a non-idempotent call, move the client's address, let it retry on its
  session; then fail the primary over and retry again. Assert one application
  and the first result each time (T10).

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
3. **The node-to-node wire format.** [§4.3](#4.3%20The%20route%20envelope) fixes what a forwarded call carries —
   the route envelope — and [§4.4](#4.4%20Node%20channels) how a channel is secured, but not the
   encoding of forwarded calls and of replication traffic ([RFC 10](rfc-10-journal-replication.md)), nor
   their versioning across a rolling upgrade.

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| Parallel file systems with separate metadata and object servers on shared hosts | roles as deployment, not code |
| pNFS flex-files and multiprotocol NAS pNFS | per-client fencing by stateid; a layout segment per byte range with its own data server, for when range shards are promoted |
| A pNFS-native NAS exporting NFSv3 and SMB from its data nodes | the protocol role belongs with the data |
| Distributed file systems publishing epoch-numbered maps | primary caches corrected by epoch refusal and pushed updates |
| Clustered SMB servers with public-address takeover, and the SMB Witness protocol | floating addresses reset by acknowledgement; clients told where to move |
