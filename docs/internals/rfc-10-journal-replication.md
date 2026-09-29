---
rfc: 10
title: "RFC 10 — journal replication"
component: journal replication
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-11-ownership]]"
aliases:
  - RFC 10
tags:
  - rfc
---
# RFC 10 — journal replication

**Status:** draft. [§15](#15.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone designing or changing how an acknowledged write survives the
loss of the node that accepted it.

---

## 1. Purpose

A write is acknowledged once the journal holds it ([RFC 8 §4.1](rfc-8-engine.md#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not)), and until it is
offloaded the journal of the node that accepted it is the only copy. On one node
that is the design. With several nodes serving one share, losing a node must not
lose acknowledged writes, and nodes other than the writer should be able to serve
recently written bytes.

This layer makes both true:

> **An acknowledged write is held by every journal in its replica set, and any of
> them can serve it once it is known to be current.**

It sits between the engine and the journal. The journal ([RFC 1](rfc-1-journal.md)) stays a local
store that knows nothing of peers; this layer decides which journals hold a file,
moves operations between them, and fences the ones that must no longer count.

### 1.1 Goals

1. **Durability across node loss.** An acknowledged write survives the loss of
   any number of nodes smaller than its replica set.
2. **Reads from replicas.** A member of a file's replica set **MAY** serve that
   file's recently written bytes from its own journal, under [§8](#8.%20Reads%20from%20replicas)'s rule.
3. **No consensus among block services.** Consensus is needed only to decide who
   owns and who replicates, and it is obtained from the configuration store
   ([§2.2](#2.2%20Roles)), which already runs it.
4. **One code path.** A single node is a replica set of one ([§10](#10.%20A%20single%20node)); the engine
   **MUST** go through this layer in every deployment.

### 1.2 Non-goals

This layer **MUST NOT**:

- decide who owns a unit, or move ownership — [RFC 11](rfc-11-ownership.md);
- order writes from more than one writer to the same bytes — one writer per
  ownership unit is assumed ([§3](#3.%20What%20it%20assumes%20of%20ownership));
- offload, carve, sync to the remote tier, or evict — the owner's engine does,
  exactly as on one node;
- replicate metadata — the metadata store has its own durability;
- erasure-code journal content ([Appendix B](#Appendix%20B%20%E2%80%94%20alternatives%20considered)).

## 2. Model

### 2.1 Terms

| Term | Means |
| --- | --- |
| **ownership unit** | the set of files one owner writes: a share by default, or a subtree ([RFC 11 §2](rfc-11-ownership.md#2.%20Ownership%20units)) |
| **owner** | the one block service that assigns versions for a unit and accepts its writes |
| **replica** | a block service whose journal holds a copy of the unit's un-offloaded operations |
| **member** | the owner or a replica; the **replica set** is the members |
| **learner** | a block service receiving the unit's operations while it catches up, before it is a member |
| **configuration** | `{unit, epoch, owner, members, learners, sealed}`, held in the configuration store and changed only by compare-and-swap |
| **epoch** | the owner epoch: raised by every configuration change, and never lower for a file than any epoch it had before ([§6](#6.%20Fencing)). The owner applies it to each file through the journal's per-file epoch ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)), so every version it assigns outranks every version assigned under an earlier epoch ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)) |
| **committed point** | per unit, the newest version at or below which every operation is held by every member. The owner computes it and sends it with every batch; it is never written to the configuration store |
| **settled point** | per file on a member, the committed point it has settled to ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)) |
| **owner lease** | the lease the owner renews in the configuration store; it is the write token of [RFC 11 §3.1](rfc-11-ownership.md#3.1%20A%20token%20is%20a%20lease%2C%20fenced%20by%20an%20epoch) — one lease, not two |
| **read lease** | a lease the owner grants a replica to serve reads ([§8](#8.%20Reads%20from%20replicas)), no longer than a configured maximum |
| **drift bound** | the configured bound on clock drift between any two nodes; every lease is reckoned with it |

### 2.2 Roles

A deployment has three roles. They **MAY** run in one process — a single node is
all three — or be split across machines.

| Role | Holds | Consensus |
| --- | --- | --- |
| protocol front-end | client sessions; forwards each operation to the owner of what it touches ([RFC 11 §5](rfc-11-ownership.md#5.%20Routing)) | none |
| metadata service | namespace and block metadata ([RFC 6](rfc-6-block-metadata.md), [RFC 7](rfc-7-namespace-metadata.md)); ownership and configurations | its store's own |
| block service | a journal per device ([§2.4](#2.4%20One%20journal%20carries%20many%20units)), the engines of its shares, carver and syncer; this layer | none |

The **configuration store** is the metadata service's store. It **MUST** provide
linearizable compare-and-swap on a configuration, and **MUST** be reachable from
every block service. Nothing in this layer depends on which store provides it.

### 2.3 What a replica holds

A replica's journal holds the unit's operations at the versions the owner
assigned, applied through `Apply` ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)) in whatever order they arrive.
Two members' journals are never byte-identical and need not be. What they agree
on is, for every byte, the newest version applied — and highest-version-wins
makes that independent of arrival order.

### 2.4 One journal carries many units

A block service keeps one journal per device ([RFC 1](rfc-1-journal.md)). That journal holds files
of every share, and every unit, the service owns or replicates. Nothing in this
layer is per journal; every rule acts per unit or per file:

- epochs, settled points, `Apply`, `Settle`, `Export` and `Discard` act per file
  ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)), so installing, settling or discarding one unit **MUST NOT**
  touch another unit's files in the same journal;
- a receiver's installed epoch is per unit, kept durably by this layer, not by
  the journal;
- the owner raises a file's journal epoch (`SetEpoch`) to its unit's epoch before
  assigning the file's first version under that epoch, and not before, so a new
  epoch costs nothing for files it never writes;
- every per-unit procedure — joining, sealing, settling, re-applying existence —
  covers only the unit's files the journal holds content for, never every file of
  the unit: a unit that is a whole share can hold millions of files;
- capacity belongs to the journal, shared among its shares with per-share
  accounting and fair limits ([RFC 1 §7](rfc-1-journal.md#7.%20Capacity)); [§5](#5.%20Offload%20and%20release) says how pressure reaches the
  owners;
- the version floor the journal is opened with ([RFC 1](rfc-1-journal.md)) covers every share
  whose files it may hold, replicated ones included.

Losing a device loses the service's membership in every unit whose files that
journal held; each unit's owner removes it under [§7](#7.%20Membership) independently.

## 3. What it assumes of ownership

[RFC 11](rfc-11-ownership.md) provides these; this layer relies on nothing else:

1. **One owner per unit at a time**, recorded in its configuration
   ([RFC 11 §2](rfc-11-ownership.md#2.%20Ownership%20units)).
2. **Every configuration change is a compare-and-swap that raises the epoch, and a
   file's epoch never decreases.** A unit's new epoch exceeds that of every unit
   that previously held any of its files; a split or a move of files between
   units is a handover whose compare-and-swap covers both units in one
   transaction ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20A%20token%20is%20a%20lease%2C%20fenced%20by%20an%20epoch), [§4](rfc-11-ownership.md#4.%20Moving%20ownership)).
3. **The owner lease** ([§2.1](#2.1%20Terms)). An owner **MUST** stop serving and acknowledging
   when its lease runs out by its own clock, reckoned pessimistically with the
   drift bound.
4. **Ownership follows the writer**, by handover rather than failover ([§9.4](#9.4%20Handover),
   [RFC 11 §3.3](rfc-11-ownership.md#3.3%20Ownership%20follows%20the%20writer)).
5. **Commits are fenced.** Every metadata commit that acts for a unit carries the
   owner epoch and is refused when it is not current ([RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency)).

## 4. The write path

The owner handles every operation on the unit. For a write, deallocate, truncate
or delete:

1. **Assign.** The owner stages the operation in its own journal, which assigns
   its version under the file's epoch ([RFC 1 §3.1](rfc-1-journal.md#3.1%20Write)).
2. **Replicate.** The owner sends the operation, with its version and the
   configuration epoch, to **every** replica in the current configuration, in
   parallel.
3. **Apply.** Each replica checks the operation against [§6](#6.%20Fencing), applies it with
   `Apply`, makes it durable, and answers.
4. **Acknowledge.** Once every member, the owner included, holds the operation
   durably, the client is acknowledged.

**Every member, not a quorum.** An operation **MUST NOT** be acknowledged until
every member of the current configuration holds it durably. That is what lets
any single member take over with every acknowledged write already in hand ([§9](#9.%20Failover)),
and what lets a replica serve reads ([§8](#8.%20Reads%20from%20replicas)). A member that cannot keep up is
removed from the configuration ([§7](#7.%20Membership)); the owner **MUST NOT** acknowledge around it
while it is still a member.

**Metadata follows the members.** Until a write's existence is committed, the
journal is its authority ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)), on every member. The owner group-commits
existence at the protocol's stability point, fenced by its epoch, and **MUST**
include only operations every member holds durably. A truncate, deallocate,
release or clone is a synchronous metadata operation: its journal operation
**MUST** likewise be held durably by every member before its metadata commit, and
the client is acknowledged after that commit.
Recorded first, a crash of the owner leaves metadata describing an operation no
surviving journal holds: a range that resolves **Lost** for a write never
acknowledged, or a removal a new owner's journal contradicts.

**A client's flush** ([RFC 8 §9.4](rfc-8-engine.md#9.4%20Commit%20is%20answered%20by%20the%20journal)) is the stability point: it is answered once
every member has synced the file's operations and their existence is committed.

**Batching.** The owner **SHOULD** batch operations to a replica and group their
syncs, as the journal groups its own ([RFC 1 §6.2](rfc-1-journal.md#6.2%20Sync%20policy)). The unit of
acknowledgement stays the operation.

## 5. Offload and release

Only the owner offloads, as on one node, and its commits carry its epoch
([§3](#3.%20What%20it%20assumes%20of%20ownership)). Replicas never offload.

**Settling.** The owner sends each member its committed point with every batch.
A member settles the unit's files it holds to it ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)), which drops removal
markers below it. A learner never settles: it cannot know that it holds
everything below the point.

**Replicas release by being told.** After an offload commit lands, the owner
sends the members the extents it covered with the commit's `oldest` and `newest`.
Each calls `MarkDurable` ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)) and then evicts under its own capacity
policy. An extent is marked only where nothing newer is held there. A notice is
fenced like any message ([§6](#6.%20Fencing)). A replica **MAY** also derive the same marks from
metadata directly, which is how it catches up on notices it missed.

**Pressure.** A replica cannot offload to make room, and its journal is shared
with every other unit it holds ([§2.4](#2.4%20One%20journal%20carries%20many%20units)). When the journal nears capacity, the
replica **MUST** tell the owner of each unit holding un-offloaded content in it,
and each owner **MUST** treat that as its own pressure ([RFC 8 §7.2](rfc-8-engine.md#7.2%20A%20capacity%20refusal%20comes%20back%20here)): offload
sooner, then refuse writes. The journal's per-share fair limits keep one unit's
backlog from refusing another's operations. A replica whose journal refuses an
operation within its share's limit is lagging for that unit, and [§7](#7.%20Membership) applies to
that unit only.

## 6. Fencing

Every message this layer sends — an operation, a committed point, a durability
notice, a read-lease grant, a seal request — carries the configuration epoch it
was sent under. **The receiver enforces it:**

- a receiver **MUST** refuse a message whose epoch is lower than the one it has
  installed for the unit;
- a receiver that sees a higher epoch than it knows **MUST** refuse the message,
  read the configuration and install it before accepting anything more.
  Installing **MUST** be durable before the receiver answers anything under the
  new epoch, so a restart never accepts what it refused before;
- a receiver that installs an epoch whose configuration does not list it as a
  member or learner **MUST** stop serving the unit's reads and `Discard` its copy
  of the unit's files, and only those ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)). It may rejoin only as a learner ([§7](#7.%20Membership));
- a receiver **MUST** refuse any operation at or below its settled point for that
  file. Every member held every operation below the committed point when the
  owner sent it, so the owner counts such a refusal as the operation held. Without
  this rule a late duplicate could reinstate content whose removal marker settling
  has already dropped;
- the metadata store refuses a commit whose owner epoch is not current ([§3](#3.%20What%20it%20assumes%20of%20ownership)).

A check made only by the sender is not a fence. An owner that pauses past its
lease, then resumes, still believes it owns the unit; what stops it is that every
receiver has installed the new epoch before the new owner accepts a write ([§9](#9.%20Failover)).

A file's epoch never decreases ([§3](#3.%20What%20it%20assumes%20of%20ownership) item 2), so anything a superseded owner manages
to write is older than everything its successor writes, including after the file
has moved to another unit.

## 7. Membership

**Replica count.** Each unit has a configured replica count and a floor. The
owner requests replicas up to the count; below the floor it **MUST** refuse
writes, and report the unit as under-replicated through share health rather than
only in a log ([RFC 0 §10.2](rfc-0-data-lifecycle.md#10.2%20No%20state%20requires%20intervention%20to%20leave)). A floor of one keeps writes available with no
redundancy; that is a deployment's choice, stated in its configuration.

**Placement.** Members of one unit **SHOULD** be in different failure domains.
What a domain is — host, rack, zone — is configuration.

**Removal.** A replica that does not answer within a configured bound, or whose
lag exceeds one, is removed: the owner writes a configuration without it at the
next epoch by compare-and-swap and installs the epoch on the remaining members.
The owner stops granting the replica read leases before the compare-and-swap,
and **MUST NOT** accept writes under the new configuration until the maximum read
lease plus the drift bound has passed since it, so the removed replica has
stopped serving reads it can no longer keep current.

**Joining.** A new or returning block service joins as a learner:

1. it `Discard`s whatever it holds of the unit's files — a returning copy may hold
   operations its configuration never acknowledged;
2. the owner streams it `Export` of every file of the unit it holds un-offloaded
   content for ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)), and
   meanwhile sends it live operations as it would a member, without waiting for
   its answer;
3. to promote it, the owner stops assigning versions for the unit, waits until
   the learner has confirmed, durably, **every version the owner assigned before
   the new epoch**, writes the configuration adding it at the next epoch by
   compare-and-swap, installs the epoch on every member, and resumes.

A learner is never counted for acknowledgement, never serves reads and never
settles. Promotion on anything less than step 3's confirmation would admit a
member lacking an acknowledged write, which then could take over and lose it.

## 8. Reads from replicas

The owner serves reads of the unit from its journal, but only content every
member holds: a read overlapping an operation still being replicated waits for
it. Otherwise a failover could remove content a client had already read.

A replica **MAY** serve a read from its own journal when both hold:

- **it holds a read lease** on the current configuration, unexpired by its own
  clock reckoned with the drift bound. A read lease **MUST NOT** outlast the owner
  lease it was granted under;
- **the range is clean**: every version it holds there is at or below the latest
  committed point it has received, and it holds no operation above that point
  that touches the range.

A range that is not clean is **dirty**: the replica asks the owner for the
newest version of the range that every member holds, and serves its own bytes
only if they carry exactly it; otherwise it forwards the read to the owner.

Why the lease: a replica removed from the configuration stops receiving
operations, and without the lease it would go on answering reads with content
that is no longer current. Removal and takeover wait out the maximum read lease
([§7](#7.%20Membership), [§9.2](#9.2%20Takeover)) for that reason.

A block service that is not a member serves the unit's bytes only under
[RFC 11 §6](rfc-11-ownership.md#6.%20Reads%20on%20non-owners)'s rule, and never serves un-offloaded content it does not hold.

## 9. Failover

### 9.1 Why one member suffices

Every acknowledged operation is held by every member ([§4](#4.%20The%20write%20path)). So any one member
that was in the configuration when the owner failed holds every acknowledged
operation, and can take over with no need to gather a quorum. What it may lack,
or others may hold beyond it, are operations never acknowledged.

### 9.2 Takeover

When the owner's lease lapses, a member takes over:

1. **Claim.** Once the owner lease has lapsed by its own clock plus the drift
   bound, a member of the current configuration writes a configuration naming
   itself owner at the next epoch. The compare-and-swap **MUST** be conditional
   on the configuration it read being current and listing it as a member, so a
   replica removed earlier can never claim. Members it cannot reach are dropped
   and must rejoin as learners ([§7](#7.%20Membership)). A member that restarted uncleanly **MAY**
   claim only once it has reopened its journal, which is durable and complete for
   everything it acknowledged.
2. **Install.** It installs the new epoch on every member it kept. From here the
   old owner can reach no member, and nothing it sends counts.
3. **Seal.** It asks each kept member which ranges it holds operations for above
   the committed point the new owner last received, and adds its own. Those are
   the **uncertain ranges**: content that may or may not have been acknowledged.
   For each, the new owner re-issues, under the new epoch, the content it holds
   there — its own bytes, or a journal removal where it holds nothing — and
   replicates it like any operation. The new epoch outranks every old one, so
   every member converges on the new owner's view of the uncertain ranges.
4. **Existence.** It re-applies existence for the unit's files it holds from its
   journal, in commits fenced by the new epoch ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)), so metadata covers
   every operation it now holds. Metadata can lag acknowledged writes ([§4](#4.%20The%20write%20path)), so
   it is the journal that decides, not the recorded size.
5. **Wait.** It waits until the maximum read lease plus the drift bound has passed
   since step 2 completed, so no replica still serves under a lease the old owner
   granted.
6. **Record and settle.** It records the seal in the configuration by
   compare-and-swap at its epoch, then settles the unit's files it holds to the
   **sealed point** — the newest version it assigned in steps 3 and 4 — and
   sends that point to every member as the committed point. Only then does it
   serve or accept writes for the unit.

Every kept member holds, at every byte, either content at or below the old
committed point or the new owner's re-issue, so settling to the sealed point is
true on each. Until it settles, the new owner cannot tell which of its content
every member holds, so it can neither offload that content nor serve it under
[§8](#8.%20Reads%20from%20replicas).

A seal not recorded is a seal that did not happen: if the claimant fails before
step 6, the next claimant repeats the procedure from its own state.

### 9.3 After takeover

The new owner offloads what it holds, which includes everything the old owner
had not. Nothing was lost as long as one member of the old configuration
survived. The old owner, if it returns, installs the new epoch on first contact,
finds itself not a member, and discards what it held ([§6](#6.%20Fencing)).

### 9.4 Handover

A planned move — ownership following the writer — does not wait for a lease:

1. the new owner becomes a member, joining as a learner and promoted under [§7](#7.%20Membership)
   if it is not one;
2. the old owner stops accepting writes for the unit, drains what is in flight,
   and sends every member the committed point covering it — the **drained
   point**;
3. it writes the configuration naming the new owner at the next epoch, by
   compare-and-swap;
4. the new owner installs the epoch on every member, settles the unit's files it
   holds to the drained point, and only then accepts writes.

No seal is needed: the old owner acknowledged nothing it had not replicated to
every member, and stopped before the move. The old owner stays a member unless
removed under [§7](#7.%20Membership). A handover that moves files between units is one
compare-and-swap over both units ([§3](#3.%20What%20it%20assumes%20of%20ownership) item 2).

## 10. A single node

On one node the unit's replica set is the node itself. Replication is a local
call, the epoch never changes, the committed point is whatever the journal has
synced, reads are always the owner's, and failover cannot happen. The engine
**MUST** still compose this layer, not the journal directly: with two code paths,
every rule in this document is exercised by one of them and not the other, and
the untested one is the one that ships to the deployment that needs it.

## 11. API surface

Signatures are indicative; the obligations above are normative.

```go
// Replicated is what the engine calls in place of the journal (§10).
type Replicated interface {
	// Each returns once the operation is acknowledgeable (§4 steps 1–4).
	WriteAt(ctx context.Context, file FileID, off int64, p []byte) (Version, error)
	Deallocate(ctx context.Context, file FileID, off, n int64) (Version, error)
	Truncate(ctx context.Context, file FileID, size int64) (Version, error)
	Delete(ctx context.Context, file FileID) (Version, error)
	// Sync returns once every member has synced the file's operations.
	Sync(ctx context.Context, file FileID) error
	// ReadAt serves under §8, or returns ErrForward naming the owner.
	ReadAt(ctx context.Context, file FileID, p []byte, off int64) (int, error)
	// Durable tells every member an offload commit covered these extents (§5).
	Durable(ctx context.Context, file FileID, ext []Extent, oldest, newest Version) error
}

// Peer is one member or learner as the owner sees it. Every call carries the
// unit and the epoch it was sent under, and the receiver fences it (§6).
type Peer interface {
	Apply(ctx context.Context, u UnitID, e Epoch, ops []Op) error // durable on return
	Commit(ctx context.Context, u UnitID, e Epoch, committed Version) error
	MarkDurable(ctx context.Context, u UnitID, e Epoch, file FileID, ext []Extent, oldest, newest Version) error
	GrantRead(ctx context.Context, u UnitID, e Epoch, until time.Time) error
	Install(ctx context.Context, u UnitID, e Epoch) error // durable on return
	Uncertain(ctx context.Context, u UnitID, e Epoch, above Version) iter.Seq2[FileRange, error]
	Newest(ctx context.Context, u UnitID, file FileID, r Range) (Version, error) // asked of the owner
}

// Configurations is the configuration store (§2.2), as RFC 11 exposes it.
type Configurations interface {
	Get(ctx context.Context, u UnitID) (Configuration, error)
	CompareAndSwap(ctx context.Context, old, next Configuration) error // ErrStale
}

type Configuration struct {
	Unit     UnitID
	Epoch    Epoch
	Owner    NodeID
	Members  []NodeID // the owner included
	Learners []NodeID
	Sealed   Epoch // the newest epoch whose takeover recorded its seal (§9.2)
}

var (
	ErrStaleEpoch      = errors.New("replication: stale epoch")
	ErrNotMember       = errors.New("replication: not a member")
	ErrSettled         = errors.New("replication: at or below the settled point") // counted as held
	ErrUnderReplicated = errors.New("replication: below the replica floor")
	ErrForward         = errors.New("replication: forward to the owner")
)
```

## 12. Invariants

| # | Invariant |
| --- | --- |
| R1 | An acknowledged operation is durably held by every member of every configuration from the one it was acknowledged under onward, until it is offloaded. |
| R2 | Existence, and every synchronous metadata operation, is committed only for operations every member holds durably; a client is acknowledged only once every member holds its operation, and after the metadata commit of a synchronous one. |
| R3 | Every receiver refuses a message whose epoch is below the one it installed, and installs durably before answering under a new epoch. |
| R4 | A file's epoch never decreases, across configuration changes and moves between units. |
| R5 | A receiver refuses every operation at or below its settled point for that file. |
| R6 | A learner is never counted for acknowledgement, never serves reads and never settles, and is promoted only after confirming every version assigned before the promoting epoch. |
| R7 | A replica serves a read only under an unexpired read lease, and only from a clean range or with the newest version confirmed by the owner; the owner serves only content every member holds. |
| R8 | Removal and takeover accept no write until the maximum read lease plus the drift bound has passed. |
| R9 | A takeover claim is a compare-and-swap conditional on the claimant being a member of the current configuration, and a new owner serves nothing until its seal is recorded and the unit's files it holds are settled to the sealed point — or, after a handover, to the drained point. |
| R10 | A receiver that installs a configuration not listing it stops serving the unit and discards its copy. |
| R11 | The engine reaches the journal only through this layer, on one node as on many. |
| R12 | Units sharing a journal are independent: every install, settle, discard, refusal and removal acts on one unit's files only. |

## 13. Observability

Every metric is labelled by unit where it is per unit, and by share.

| Answers | Metric | Type |
| --- | --- | --- |
| committed-point lag: versions and time between the newest assigned and the committed point | `dittofs_replication_committed_lag_seconds` | gauge |
| time replication adds to an acknowledgement | `dittofs_replication_ack_seconds` | histogram |
| members and learners per unit; below the floor raises a health condition | `dittofs_replication_members` | gauge |
| refusals, labelled `reason` = `stale_epoch`, `settled`, `not_member` or `pressure` | `dittofs_replication_refusals_total` | counter |
| configuration changes, labelled `kind` = `takeover`, `handover`, `remove` or `join` | `dittofs_replication_config_changes_total` | counter |
| takeover time, lease lapse to writable | `dittofs_replication_takeover_seconds` | histogram |
| seal duration, and the uncertain bytes it re-issued | `dittofs_replication_seal_seconds`, `dittofs_replication_seal_bytes_total` | histogram, counter |
| learner catch-up time | `dittofs_replication_catchup_seconds` | histogram |
| replica reads, labelled `result` = `clean`, `confirmed` or `forwarded` | `dittofs_replication_replica_reads_total` | counter |

Logs: every configuration change at `Info`, with unit, old and new epoch and
reason. An owner that stops on its own lease expiry logs at `Error`. Stale-epoch
refusals log at `Warn`, rate-limited per sender.

## 14. Test plan and benchmarks

This layer is a distributed protocol, and no amount of review establishes one.
Conformance rests on three kinds of check, and all three are required.

**A model.** The configuration, fencing, takeover and seal of [§6](#6.%20Fencing)–[§9](#9.%20Failover) **MUST**
be modelled in a model checker before they are implemented, with the invariants
of [§12](#12.%20Invariants) as properties, and the model **MUST** be kept in step with this document.

**Deterministic simulation.** The layer reaches the network, the journals' storage
seam ([RFC 1 §1.3](rfc-1-journal.md#1.3%20It%20is%20testable%20on%20its%20own)), time and randomness only through interfaces. A simulator
drives a cluster of instances in one process from a seed, injecting message loss,
delay, reordering and duplication, partitions, process pauses, crashes that
discard unsynced writes, and clock drift within the configured bound. A failing
seed **MUST** reproduce the failure exactly.

**Whole-system fault injection** against real processes, checking the histories
clients observe for linearizability of acknowledged writes.

Properties every one of them checks:

| Property | Violated by |
| --- | --- |
| An acknowledged write is held by some journal of the current configuration, or durable remotely, at every moment (R1) | a quorum acknowledgement, a seal that drops content, a learner promoted early |
| No journal serves, for a range, content older than the newest acknowledged write there, and no read returns content later removed by a seal (R7, R8) | a removed replica still serving, a dirty range served as clean, an owner serving unreplicated content |
| Removed content is never served again (R5) | a late duplicate below a settled removal, a rejoining copy that did not discard |
| Nothing a superseded owner sends after the epoch is installed takes effect anywhere (R3, R4) | a sender-side lease check standing in for receiver fencing, an epoch that fell on a move |
| A removed replica never becomes owner (R9) | a claim not conditional on membership |
| A unit with a member of the last configuration alive becomes writable again without operator action | a takeover that waits for a quorum |
| A new owner serves and offloads only settled content (R9) | serving before settling, trimming to a metadata size that lags acknowledged writes |
| One unit's install, discard, lag or pressure never changes or refuses another unit's files in the same journal (R12) | a discard by journal rather than by file, one share's backlog refusing another's writes within its fair limit |

**Benchmarks**, on three block services on one local network, each the reference
box ([Test tiers](rfc-index.md#Test%20tiers)):

| Benchmark | Measures | Target |
| --- | --- | --- |
| Committed-point lag | p99 under sustained 4 KiB writes, three members | ≤ one sync interval + 2 ms |
| Acknowledgement overhead | p50 and p99 write latency against a single node | ≤ one network round trip + one member sync |
| Takeover time | lease lapse to writable, 64 MiB uncertain, 10³ files | ≤ read lease + drift bound + 1 s |
| Seal duration | seconds per uncertain GiB | report; ≤ 1 s for 64 MiB |
| Handover time | unit with 64 MiB un-offloaded | ≤ 2 s at 10 Gb/s |
| Learner catch-up | MB/s of `Export` applied | ≥ 80% of the link |
| Whole-cluster restart | time until 10⁴ units are writable | report ([§15](#15.%20Open%20questions) item 3) |

## 15. Open questions

1. **The seal's cost.** Asking every member for its uncertain ranges is
   proportional to operations above the committed point; the bound on that — the
   committed point's lag — is unmeasured.
2. **Clock assumptions.** The read lease and the owner lease assume bounded
   drift. The bound, and what happens when it is exceeded, are unspecified.
3. **Whole-cluster restart.** Every lease has lapsed; every unit needs a takeover
   before it serves. Whether that is acceptable at 10⁴ shares is unmeasured.
4. **Journal capacity** is multiplied by the replica count for un-offloaded
   content, on journals shared by many units. Whether offload keeps that bounded
   under sustained writes is unmeasured.

Per-range units and protocol state are [RFC 11](rfc-11-ownership.md)'s.

---

## Appendix A — prior art

Every system below that keeps consensus off the data path does it the same way:
one writer orders each unit, a service that already runs consensus holds the
configuration and an epoch, and every message is fenced by it. This design is
that pattern.

| System | What this design takes from it |
| --- | --- |
| PacificA (Lin et al., MSR-TR-2008-25) | configuration kept apart from data replication; commit needs every replica; a new primary reconciles before serving |
| Windows Azure Storage stream layer (Calder et al., SOSP 2011) | acknowledgement from all replicas; a failed writer is sealed rather than repaired, and appends continue under a new configuration |
| Chain replication (van Renesse, Schneider, OSDI 2004); CRAQ (Terrace, Freedman, USENIX ATC 2009) | reads from replicas: clean ranges served locally, dirty ones checked with the writer |
| Kafka, KIP-101, KIP-279, KIP-966 | truncation by epoch, never by a local watermark; a replica that restarted uncleanly is not a takeover target until caught up |
| Aurora (Verbitski et al., SIGMOD 2017, 2018) | a single writer's versions make replica acknowledgements consistent; a recovery's truncation recorded durably under its epoch |
| GFS (SOSP 2003), HDFS lease recovery | the configuration version bumped before a new writer writes; stale replicas detected by version |
| Ceph peering | a new primary records that it is active before serving, or a short-lived primary can acknowledge writes nobody finds |
| Frangipani (SOSP 1997) | a lease checked only by the sender is not a fence: storage must reject a stale writer |
| Assise (OSDI 2020) | a local log replicated before acknowledgement and published asynchronously — the same shape as journal plus offload |
| FoundationDB (SIGMOD 2021), TigerBeetle | deterministic simulation as the primary test method |

## Appendix B — alternatives considered

| Alternative | Why not |
| --- | --- |
| Every block service writes, and journals order operations differently | Two writers of one range leave replicas holding different bytes with no version comparable across them. Ordering needs one writer per unit or consensus per write. |
| Consensus per write through the configuration store's timestamp service | Moves consensus from rare events to every write. Reads of recent data then need a quorum or a metadata lookup, several nodes offload overlapping content, and `size` becomes contended. It buys nothing over forwarding to the owner, which costs the same hop the replication already pays. |
| Acknowledge at a quorum of the replica set | Hides a slow replica, but a replica can then miss acknowledged writes, so none can serve reads, and failover needs a read quorum and a durable truncation record. |
| Replicas read another's writes only after offload | Metadata shows a new size at once; reading the un-offloaded range on another node must then wait for offload or fetch from the owner anyway. It also breaks close-to-open visibility unless close waits for offload. |
| Erasure-coded journal content | Journal content is small, overwritten and short-lived: stripes need read-modify-write on partial writes, failover must reconstruct, and no replica holds whole data to serve reads. Erasure coding belongs at rest, where blocks are sealed. |
| A shared journal on storage every node can reach, or a replicated log service | Either requires shared-access storage or another service to deploy and keep healthy. Replication among block services needs neither. |
| Journal in a key-value store or an in-memory cache | Weaker durability or capacity, write amplification on large values, and the journal's semantics rebuilt on top. |
