---
rfc: 27
title: "RFC 27 — moving a namespace between installations"
component: migration
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-9-gc]]"
  - "[[rfc-10-journal-replication]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-26-catalog-backups]]"
aliases:
  - RFC 27
tags:
  - rfc
---

# RFC 27 — moving a namespace between installations

**Status:** draft. [§7](#7.%20Open%20questions) lists what is undecided.
**Audience:** anyone implementing the namespace claim, moving a namespace
between installations that share one bucket, or re-homing a share out of a
shared namespace. Snapshots, which a move reads, are [RFC 12](rfc-12-snapshots.md);
the export format a move writes is [RFC 26](rfc-26-catalog-backups.md).
Conventions and test tiers are in [the RFC index](rfc-index.md).

This document specifies behaviour, not the current code. [Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists
where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

**What this is.** A **namespace** is the folder in a bucket a share's blocks live
in, and the unit that moves ([RFC 12 §2.1](rfc-12-snapshots.md#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)).
Exactly one installation holds a namespace at a time, proven by a **claim**
object in its folder. A **move** hands a whole namespace to another installation
that reaches the same bucket: the metadata and the right to collect garbage
move, and no block is copied. A **re-home** copies one share out of a namespace
it shares with others into a namespace of its own, while it serves, so that it
can then move alone.

**The problem, with one example.** A hardware refresh moves namespace
`ns-photos`, with share `photos` (10⁷ files), from installation A to
installation B; both reach the bucket `dfs-data`.

1. **Pre-seed, while A serves.** A takes a base snapshot of the share, held by a
   use record, and writes the namespace's **GC pause** record, so A relocates
   and deletes nothing there from now on. A exports everything the base sees as
   a `move-base`; B stages it, rebuilds its indexes and recomputes its counts,
   and publishes nothing. This takes hours; clients keep writing to A.
2. **Freeze.** A closes the share's gates, offloads everything dirty, and stops
   its writers and GC. Calls arriving now are answered "retry later".
3. **Delta.** A exports only the records changed since the base, as a
   `move-delta`; B stages it over the base and reports ready.
4. **Release and claim.** A writes the claim `released`, naming the digest of
   what it exported. B publishes in one transaction, writes the claim `owned`,
   and serves; clients reconnect to B.
5. **Drop.** A deletes its records of the namespace without releasing a ref or
   deleting a remote object: the blocks are B's now.

```text
 A:  serve ── base cut, GC pause ── move-base ──► │ freeze ── move-delta ──► claim released ── drop
 B:                                 stage, index  │          stage, ready     publish, claim owned, serve
 bucket dfs-data: untouched throughout — no block is copied
```

**The words you need.**

- **Claim** — the control object in a namespace's folder naming the one
  installation that may write and collect there ([§2.1](#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)).
- **Change sequence** — the commit timestamp the metadata store keeps with each
  key version; a move's delta is the records stamped above the base's
  ([§2.2](#2.2%20The%20move%2C%20step%20by%20step)).
- **GC pause** — the namespace's durable record that stops relocation, deletes
  and collection while a move or a recovery runs ([§2.2](#2.2%20The%20move%2C%20step%20by%20step)).
- **Re-home** — copying one share's content into a new namespace of its own,
  while it serves ([§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)).

**What this RFC promises.**

- One installation holds a namespace; only it puts, sweeps, relocates or
  collects there, and a copy of an installation's disks never silently becomes a
  second holder.
- Moving a namespace to another installation on the same bucket copies no block,
  and the freeze carries only what changed since the pre-seed.
- A move never releases refs at the old installation and never deletes a remote
  object on its behalf.
- A re-home keeps the share serving, and changes no snapshot's or live read's
  bytes.

**How the rest is organised.** §1 lists what moves are for and what they are
not. §2 is the core: §2.1 the claim, §2.2 the move step by step (read these),
then GC across installations, key scope, versions and FileIDs on import,
replication, and the re-home (§2.7, long and self-contained). §3 is the API and
configuration, §4 the invariants, §5–§6 tests and metrics, §7 the open
questions.

## In short

- **A namespace** is a folder in a bucket, and each share gets its own by
  default. One installation holds it, named by the namespace's **claim**.
- **Migration** hands a whole namespace to another installation on the same
  bucket without copying a block: the target is seeded while the source keeps
  serving, and a short freeze carries only what changed since.
- **GC never runs for one namespace at two installations.** The move pauses the
  source's relocation and deletes durably, and the target takes over the backlog.
- **A re-home** copies one share's content out of a namespace it shares with
  others, into a new namespace of its own, while the share keeps serving.

## 1. Purpose

Operators ask a file service's data protection to **move tenants between
installations.** Hardware refresh, rebalancing, or outgrowing a single-node
installation all mean moving shares that can hold petabytes. Because both
installations can reach the same bucket, a move
([§2](#2.%20Moving%20a%20namespace%20between%20installations)) transfers the metadata and the right to collect garbage, and never
the data. A share that shares its namespace with others is first re-homed into
one of its own ([§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), which does copy its data.

A move reads a snapshot ([RFC 12](rfc-12-snapshots.md)), writes the export format
of [RFC 26 §3](rfc-26-catalog-backups.md#3.%20The%20export%20format), and is
composed from what the set already has: counted history refs
([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)) and GC as one
service per namespace
([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix)).

### 1.1 Non-goals

This document **MUST NOT** be read as specifying:

- two installations serving one namespace at once. That is replication and
  sharding ([RFC 10](rfc-10-journal-replication.md), [RFC 11](rfc-11-ownership.md)); [§2.6](#2.6%20After%20replication)
  says how the two relate.

### 1.2 Terms

Share, namespace, installation, shard, primary, epoch, journal, ref, chunk,
block, cut and snapshot are defined once, in
[RFC 0's glossary](rfc-0-data-lifecycle.md#Glossary); cut number, `born` and
`died`, history and use record are
[RFC 12 §1.2](rfc-12-snapshots.md#1.2%20Terms)'s; export, import and copying
backup are [RFC 26 §1.2](rfc-26-catalog-backups.md#1.2%20Terms)'s. This document
adds:

| Term | Means |
| --- | --- |
| **change sequence** | the commit timestamp the metadata store keeps with each key version a transaction writes under a share's prefixes, or its namespace's content-addressed ones, ordered like the commits and returned by a scan; no record holds it in its value ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)); a move's delta is the records stamped above the base's ([§2.2](#2.2%20The%20move%2C%20step%20by%20step)) |
| **claim** | the control object in a namespace's folder naming the one installation that may write and collect there ([§2.1](#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) |
| **re-home** | copying one share's content from the namespace it shares into a new namespace of its own, while it serves; a ref names its namespace by **generation** during one ([§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |

## 2. Moving a namespace between installations

### 2.1 One installation per namespace, proven by a claim

Exactly one installation holds a namespace: only it puts, sweeps, relocates or
collects there, and only its metadata store holds the namespace's records. Which
one is in its configuration and in the **claim**: the control object of role
`claim` ([RFC 4 §4.13](rfc-4-remote-tier.md#4.13%20Control%20objects)) in the namespace's folder, never listed as a block,
like the health object ([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)). It holds the installation's identity,
the claim epoch, the holder's **instance nonce**, and state `owned` or
`released` (with the digest of the move export).

**An installation's identity changes when it is copied.** The identity minted
when the installation is created ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)) is half of it; the other half
is an **instance** part, recorded with it, that a copy of the installation's
disks does not keep. At every start the installation reads the platform's
machine-generation identifier, where the platform offers one that changes when
a virtual machine is cloned or restored from an image, and compares it with the
one it recorded. A difference means this process runs on a copy: it mints a new
instance part, and holds no namespace — it does not write, collect or delete in
any — until an operator states which of the copies is the installation. A copy
is never silently a second holder. A copy may hold a metadata store older than
the one last served — a VM restored from an image — and nothing records that
it is older, so once the operator confirms a copy, or the nonce check
(below) has stopped one, the confirmed installation runs
[RFC 9 §3.7](rfc-9-gc.md#3.7%20Trash)'s settle of retired blocks before it serves, adopts, clones or
collects, as for any store opened at an older state.

**The claim catches a copy the platform does not report.** At every start, and
at every `Recheck`, the holder rewrites its claim with a fresh instance nonce, in
this order, each step done before the next:

1. **read** the claim. If it names this installation's identity with a nonce
   other than the two this installation recorded — the last it wrote and the one
   it intended to write next — the reader stops (below), and writes nothing;
2. **record** the new nonce as the intended one, committed in its own metadata
   store, beside the instance part ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities));
3. **put** the claim with that nonce, then record it as the last written.

A claim read that names this installation's identity but carries a nonce it did
not record means another process with the same identity is writing it: the
reader **MUST** stop writing to the namespace, and its GC **MUST** issue no
further delete, at once, and alert. Reading before rewriting is what lets the
original win against a stale image: a disk image restored from yesterday records
yesterday's nonces, finds the original's newer one at its first start, and stops
before its own put; rewriting first would let the stale copy overwrite the
claim and fence the original instead, after which its GC deletes blocks only the
original's newer records name. Recording before the put is what lets a holder
that crashed between the two recognise its own claim at restart. Two copies
started at once from one image fence each other within one `Recheck` period: the
one that reads the other's nonce stops, and only one keeps writing.

> decision: which copy survives is decided by timing, not by which is the
> original, and the alert names both so an operator can swap them. The cost is
> one claim put per namespace per `Recheck` period. Make the claim a conditional
> write ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)) if a fleet's claim puts show in its request costs.
Reading it is a step of the store's capability check
([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)). Installations that share only the bucket check each other
through it:

- GC **MUST** read the claim at every `Recheck` and before each batch of deletes,
  and **MUST** issue no delete while the claim does not name its installation as
  `owned` ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)). A GC process **MUST NOT** issue a delete more than
  two `Recheck` periods after it issued its last read of a claim naming it, by its
  own clock — from when the read was sent, not when it returned, so a slow read
  cannot stretch the window.
- An installation **MUST** open a namespace for writing only while the claim names
  it `owned` and its own record of the namespace is `serving`, and **MUST** stop
  writing when a claim read on the `Recheck` period says otherwise.
- An import **MUST** take the claim only from a claim that is `released` with the
  digest of the export being imported, or, for a recovery import ([RFC 26 §2.3](rfc-26-catalog-backups.md#2.3%20Restore)), on
  the operator's statement that the installation holding it is gone. It writes the
  claim `owned` at the next epoch. After a recovery import it then waits
  *T* × (1 + ρ) + σ, with *T* two `Recheck` periods and ρ and σ the clock-rate
  and clock-offset bounds of [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile), before it puts, adopts, resolves a
  hint or deletes, so that a partitioned old installation's GC, counting its two
  periods on a clock that may run slow, has fenced itself.

**Every claim put records its nonce first.** Every path that writes a claim — a
start, a `Recheck`, a move's or a recovery's import, a re-home's new namespace
([§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), a restore into a new namespace ([RFC 26 §2.4.5](rfc-26-catalog-backups.md#2.4.5%20Restore%20into%20a%20new%20namespace)) — records the nonce it
intends to write in its own metadata store before the put, and records it as the
last written after, as steps 2–3 above do. The record of a namespace's claim
nonces (`NS‖ns‖claim`, [RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)) is installation-local and **MUST
NOT** be exported: an import that carried the old holder's nonces would find its
own claim naming a nonce it never recorded at its first `Recheck`, and fence
itself.

> decision: the claim is a check, not a lock: the remote contract has no
> conditional put, so two installations that both believe they hold a namespace
> can both write it. The order of [§2.2](#2.2%20The%20move%2C%20step%20by%20step) prevents that, and the
> self-fence bounds a partitioned one by clocks within [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)'s clock
> bound, which rests on the hosts' time synchronisation. Make the claim a lock if
> the contract gains a conditional write.

### 2.2 The move, step by step

Until replication spans installations, a migration is a move, from installation A
to installation B. The bulk of the metadata is copied while A keeps serving; the
freeze carries only what changed since.

![Moving a namespace](img/rfc12-migration.svg)

1. **Pre-flight.** A writes the export's header alone; B checks prefix, key scope
   and material against it ([§2.4](#2.4%20Key%20scope%20and%20material)), and that it configures every backup
   location a use record or a folder record ([RFC 26 §2.4.1](rfc-26-catalog-backups.md#2.4.1%20Layout%20at%20the%20location)) of the namespace
   names, whose expiry and sweeps it will run.
2. **Pre-seed, while A serves.** A takes a **base cut** of every share in the
   namespace: an ordinary snapshot held by a use record of kind `move`
   ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)), not by a lock, so the move can delete it. A then writes the
   namespace's **GC pause** record, which GC reads before every pass and every
   batch and which survives a restart: a paused namespace gets no relocation,
   delete or collection. Every relocation commit, every transaction that marks a
   block deleted, and every retirement of an unrecorded object a listing found
   reads the pause record with conflict tracking, so writing it aborts each one
   not yet committed; one already committed is in the records the export reads,
   its old block's delete waiting with every other delete to become B's
   backlog. A cooperative move therefore needs no clock: the claim's self-fence
   ([§2.1](#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) is only the backstop for a process that does not see the pause. Retirement is not
   paused: it is decided where a count reaches zero
   ([RFC 9 §2.2](rfc-9-gc.md#2.2%20Retirement%20is%20decided%20where%20the%20count%20reaches%20zero)), an adoption undoes it, and the delta carries the
   records it changes. Once every base cut is `complete`, so that no offload can
   still write a ref a base cut sees, A writes an export of kind `move-base`: every
   record the base cuts see, all history, every chunk and block record, and the
   principals they name. B stages it, rebuilds its derived indexes — reverse ref
   keys, the died index, the version-floor index, the GC index — and recomputes
   its counts as it stages, and publishes nothing. From here until the
   move ends, the namespace refuses share creation, share deletion, clones,
   backups of either kind and re-homes ([§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) with `ErrMoving`, and defers snapshot deletion and pruning; a deletion,
   clone or backup already running finishes first.
3. **Pre-drain.** A expedites offload until the namespace's dirty and held bytes
   would drain within half of `migration.freeze_timeout` at the measured offload
   rate.
4. **Freeze, durably.** A records the namespace as `moving`, which keeps it closed
   across a restart; closes every shard's cut gate on every share and keeps it
   closed; pauses removal batches; and offloads everything dirty or held, since B
   cannot read A's journals. An operation that arrives during the freeze waits
   at most `migration.hold_reply`, then is answered with the retry-later error
   `ErrDelay` ([RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model)), which NFS clients receive as `NFS4ERR_DELAY` or
   `NFS3ERR_JUKEBOX` and retry. An SMB request is kept pending with an interim
   response instead, under the same hold an adapter keeps for a request refused
   `ErrGrace` ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)), bounded by `migration.freeze_timeout` plus
   35 s; if the namespace has moved when the freeze ends, the connection is
   dropped so durable handles reconnect at the new owner. No NFS call is held for
   the whole freeze, and none is failed outright. The freeze **MUST** be bounded by
   `migration.freeze_timeout`; one that cannot finish in time aborts the move.
5. **Stop A's writers and GC** for the namespace, and join them: offload loops,
   relocation, the deleter, collection ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)).
6. **Delta.** A writes an export of kind `move-delta`, naming the base's digest:
   - every record whose change sequence is above its base cut's — live or
     history, per-file or per-share, and every chunk and block record above the
     namespace's. `born` cannot select them: an offload that commits late writes a
     live ref with `born` below the base, and a narrowed ref keeps its `born`
     ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). Nothing the base saw was deleted meanwhile: a live base cut
     turns every drop of it into a move to history, and the paused GC deletes no
     chunk or block record;
   - every record under each share's prefix — snapshots, cuts, grants, quotas,
     usage, locks and use records — and every per-file record that carries no
     `born` — removals, pending releases, durable opens — which B takes in place
     of the base's, so a deletion among them carries too;
   - every put intent ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)), and the principals the delta names.
7. **Ready at B, then release.** B stages the delta over the base
   ([RFC 26 §3.2](rfc-26-catalog-backups.md#3.2%20Import%20is%20staged%20and%20published%20atomically)), applying its changes to the indexes and counts it built from
   the base, verifies it, and reports **ready**: everything but the publish is
   done. Only then does A write the claim `released` with the digest over base
   and delta, and record the namespace `released`. Everything up to ready runs
   inside `migration.freeze_timeout`, and a failure or a timeout before it
   aborts the move as below; no unbounded or unabortable step follows the
   release.
8. **Claim at B.** B publishes the staged import — one transaction — writes the
   claim `owned` at the next epoch, and starts the shares. Clients reconnect to
   B.
9. **Drop at A.** A first calls `Forget` for each of the namespace's shares'
   tags in every journal that holds one ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)), and makes no per-file
   `Delete`: one header-only record per tag that drops every extent, removal
   marker and hold mark of its files — every record of the tag below the forget
   record's sequence number, whatever its version — which no caller settles and
   `Since` never yields. So no extent or marker of a
   moved file stays in A's journals, and a later move back finds none to replay
   as a removal. Step 4 offloaded all of
   them, so this discards nothing B lacks. A then deletes the namespace's records
   from its metadata store **without releasing them**: no refcount is
   decremented, no GC index key is written, nothing is swept, and nothing in the
   remote store is deleted. A crash between the two resumes the `Forget` calls
   at A's next start, before anything is served, from the namespace's
   `released` record.

B drops the GC pause record at publish, then deletes each move use record and its
base cut as an ordinary snapshot. The freeze lasts the drain plus the delta. The
delta exports only the changes since the base, but selecting them is one
sequential scan of the namespace's keys filtered by change sequence: metadata
reads only, no remote I/O.

> ponytail: the delta is found by a filtered scan, O(the namespace's keys), and
> sent in O(changes). Upgrade to a per-namespace change log written with each
> commit when the freeze's scan time shows in a move benchmark.

The pause from step 2 grows the namespace by what relocation and deletes would have reclaimed during the
pre-seed.

**Aborting.** Before A writes the claim `released` in step 7, A **MAY** abort: it records the namespace `serving`,
deletes the GC pause record, deletes each move use record and then its base cut,
and reopens the gates; B drops its staging.
After that write, A **MUST NOT** re-claim, reopen or write the namespace unless the
operator states that B has not published the import, which A cannot learn itself;
it then writes the claim `owned` at a higher epoch. A move keeps share identities
and FileIDs ([§2.5](#2.5%20Versions%20and%20FileIDs%20on%20import)), so clients' handles **SHOULD** stay valid at B.

**Worked example: a pre-seeded move** — `S-snap-move-preseed`. Namespace
`ns-photos` holds share `photos`, 10⁷ files, one shard, at cut 19.

| t | A | B | clients |
| --- | --- | --- | --- |
| 0 | pre-flight header | checks scope, prefix, material: ok | writing to A |
| 1 | base cut 20, held by a move use record; GC pause record written, one relocation in flight aborts; 20 `complete` after 40 s; `move-base` export, 10⁷ files, 3 h | stages it | writing to A throughout |
| 2 | pre-drain: 40 GiB dirty down to 2 GiB | — | writing to A, slower |
| 3 | `moving`; gates closed; drains 2 GiB in 20 s; joins writers and GC | — | calls answered retry-later after 1 s, and retried |
| 4 | `move-delta`: 3×10⁴ records with change sequence above cut 20's — among them the kept part of a ref a truncate narrowed at t1, still `born` 12 — all share-prefix records, intents; 5 s | stages it over the base, applies it to the indexes and counts built at t1, verifies; ready | calls wait |
| 5 | claim `released` with the digest | — | calls retried |
| 6 | — | publishes; claim `owned` at epoch 4; serves | reconnect to B: 40 s after t3 |
| 7 | drops its records | drops the pause record; deletes the move use record, then base cut 20 | — |

### 2.3 GC across installations on one bucket

No namespace is ever served by two GC services:

- **The deleter** acts only on blocks its own store records, and only after
  checking its own reverse ref index ([RFC 9 §2.3](rfc-9-gc.md#2.3%20The%20absence%20of%20a%20record%20proves%20nothing),
  [RFC 9 §3.5](rfc-9-gc.md#3.5%20The%20deleter%20verifies%20before%20it%20deletes)). In a move A's relocation and deletes are paused by the
  durable pause record from the pre-seed and its GC joined before release, and A records nothing after step 9; B counts every ref and writes
  its reverse keys, since every share and snapshot moved.
- **Retired and deleted blocks, and intents,** move too. B rebuilds the GC index
  from the block records ([RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)) and resumes them as its own trash and
  delete backlog, keeping each `not_before` ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)); a retired block's
  chunk records move with it, so an adoption at B still resurrects it. Every
  imported intent was written by A, whose writers stopped and were joined at
  step 5, so none can still be followed by a commit: B treats each as abandoned
  ([RFC 9 §3.4](rfc-9-gc.md#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete)) and collects it with its object, if any
  ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)). No epoch comparison is involved.
- **Collection** ([RFC 9 §5.3](rfc-9-gc.md#5.3%20It%20runs%20only%20where%20the%20namespace%20is%20proven)) runs only where claim and configuration agree;
  objects A put and never committed are B's to collect.
- **A stale process of A** that puts after the release only leaks: its commit
  finds no intent, and the object is left to B's listing backstop. One that
  deletes is stopped by the claim check before each batch and by its own fence
  two `Recheck` periods after its last good claim read ([§2.1](#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)). A process
  that escapes both — paused past the clock bound between its check and its
  delete — can delete a block B resurrected or minted; that is the ceiling the
  claim's decision states.

A namespace **MUST NOT** be split between installations ([RFC 12 §2.1](rfc-12-snapshots.md#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)).

### 2.4 Key scope and material

A namespace's key scope, and every other bound setting of it
([RFC 13 §5.1](rfc-13-configuration.md#5.1%20A%20bound%20setting%20refuses%20change)), is fixed when it is created and travels in every export's
header ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)): the prefix, the key scope, the chunking target and
bounds, whether it encrypts, the IDs of its chunk-ID and chunking keys, and the
block format version it writes. B **MUST** use them for the namespace, and
**MUST** refuse with `ErrScopeMismatch`, naming the setting, an import any of
whose bound settings differs from its configuration: names derived under
another scope would never match the imported records ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), another
chunking target would re-cut every file B writes against content cut the old
way, and a namespace that encrypts at A must not take plaintext writes at B.
Each share's fold rule and entry-digest hash key travel the same way, and B
**MUST** create the share with them, never draw its own: every entry key and
every client's listing cookie was derived under them
([RFC 7 §3.4](rfc-7-namespace-metadata.md#3.4%20Enumeration)).

The export lists every material ID in the census of the blocks it names, each
with its fingerprint, and B **MUST** find each held by its provider, with the same
fingerprint, before staging a record ([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)): material unavailable
now, or unknown to B's provider, refuses the import as retryable, naming the IDs
— loading the keys ends it; destroyed material refuses it for good, naming the
IDs. An import **MUST NOT** proceed and leave reads to fail as corrupt.
B needs no particular *current* material: its own chain writes new blocks
([RFC 5 §2.8](rfc-5-transforms.md#2.8%20The%20chain%20ID)).

The namespace's **data keys**, **header keys**, **chunk-ID key**, **chunking
key** and **export keys** are kept as key records, each held as
[RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)'s table says for the namespace's kind: wrapped under a
master key, except a non-encrypting namespace's chunk-ID key, kept in the clear
beside its blocks. The export carries every key record so held, and the IDs of
the master keys that wrap them, and never a master key. B **MUST** hold every
master key the export names, and each key record **MUST** unwrap and match its
fingerprint, before staging: without the
export key it cannot read the export, without the chunk-ID key it cannot verify a
chunk, without a header key it cannot verify A's block headers, and without the
chunking key its new writes chunk differently from every block A wrote.

**B re-wraps what it imports; A keeps what it exported under.** At publish, B
**MUST** re-wrap every imported key record under its own current master key
([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)), in the publishing transaction, so after a move no key record
of B's depends on a master key A may rotate away and destroy. A **MUST**, when
it writes a move's export, record a durable **export pin** naming each master
key the export's wrappings use, and its material provider **MUST** refuse to
destroy a pinned master key with `ErrMaterialInUse` until an operator releases
the pin: A cannot see B's re-wrap, and B's retained exports from before its
re-wrap still name it. The destroy check runs per installation, against that
installation's own key records, retained exports and pins.

### 2.5 Versions and FileIDs on import

**A journal attaching an imported share holds none of its extents.** A share
moved back to an installation that once held it — A to B and back to A — arrives
with FileIDs A's journals may have held. Attaching a share whose tag the journal
already knows as an import ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)) **MUST** first `Forget` that tag —
dropping its extents and markers through one header-only record — or be refused
naming the tag; otherwise A would serve its stale,
clean extents keyed by those FileIDs over what B wrote since. Step 9's `Forget`
makes this the rare case of a crash, not the rule.

**Versions.** A version B's journal assigns to an imported file **MUST** exceed
every version imported for it, or a new write loses precedence to older content
and is dropped at its commit ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)). The journals at B were opened
before the share existed there, so the floor they opened with does not cover it.
So before a journal serves a share it did not serve when it opened — an imported
share of a move, a recovery, a clone or a restore — it **MUST** raise its version
counter above that share's version floor: the highest version B's metadata now
records for any file of the share, every ref's `newest` and every FileData's
`applied` ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)). The publish makes that floor readable; the
share's first write waits for the raise. Journals opened later include the
share in their floor at open, as for any share ([RFC 1 §9.1](rfc-1-journal.md#9.1%20Rebuilding)). The export
needs no version field of its own: the floor is computed from the records it
imported. A move keeps every `born` and `died`, each snapshot's cut number and
each share's `Cut` record: they are share-local and involve no journal version.
A clone or restore has no snapshots and no history, writes every record with
`born` 0, and starts at cut 0.

**File numbers.** A move, a clone and a restore keep each file's `Number`
([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)). The transaction that publishes an import **MUST** raise the
share's file-number allocator above the highest `Number` it imported, before any
create in the share: otherwise B's next create is issued a number a restored
file holds, and two files report one id. A move carries the allocator record
itself, and the raise then changes nothing.

**FileIDs.** A FileID is unique across a store and keys content in every journal
([RFC 0 §3](rfc-0-data-lifecycle.md#3.%20Identity)):

- a **move** keeps FileIDs and share identities, and **MUST** be refused if any
  imported FileID already exists at B;
- a **clone** or **restore**, and each detached snapshot a recovery imports, gets
  new FileIDs and a new share identity, since one snapshot may be restored many
  times.

**Handles cannot alias across shares.** Every per-file key carries its share's
identity ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), and a handle names the share and the file
([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)). A clone or restore gets a new share identity, so no handle to
the source can resolve into it.

### 2.6 After replication

**(cluster)** Once replication and sharding span installations, two installations **MAY** serve
one namespace under [RFC 10](rfc-10-journal-replication.md) and [RFC 11](rfc-11-ownership.md)'s rules — one
metadata store, primaries fenced by epoch, handover
([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)) in place of a move. This document does not specify that;
the move of [§2.2](#2.2%20The%20move%2C%20step%20by%20step) remains the way to change which metadata store holds a
namespace.

### 2.7 Moving one share out of a shared namespace

A share in a namespace shared with other shares — a clone with its source, while
deduplication is deferred ([RFC 12 §2.1](rfc-12-snapshots.md#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)) — cannot be moved alone. Its chunks are
counted together with theirs. A **re-home** fixes that: it copies the share's
content into a new namespace of its own while the share keeps serving. After
that, the share moves alone ([§2.2](#2.2%20The%20move%2C%20step%20by%20step)). A re-home is the migration
[RFC 13 §5.1](rfc-13-configuration.md#5.1%20A%20bound%20setting%20refuses%20change) names as the only way a share's namespace binding changes while the
share holds content, and so also the only way to change a bound setting — the
chunking target, the keys, whether the namespace encrypts — for content already
written: the new namespace is created with the new settings. A share that is its
namespace's only share is re-homed for that reason alone.

> decision: shares created into an existing namespace are deferred with
> deduplication, but re-home is not: it is how a clone leaves its source's
> namespace and how any share changes a bound setting. Multi-share namespaces
> built for deduplication return, with what re-home must do for them, when the
> dedup lookup does.

A re-home is a data copy. Every chunk the share's refs name, live and history, is
read from the old namespace N and written into new blocks in the new namespace N'.
Deduplication against N's other shares is lost by design. A chunk they also hold
ends up stored in both namespaces. Within the share, deduplication is kept: N'
stores each of its chunks once.

**A ref names its namespace by generation.** A share's record lists its namespaces
by **generation**: one entry outside a re-home, and two during one, N at *g* and N'
at *g* + 1. Every ref carries the generation of the namespace its chunk is counted
in. A read, an adoption or a drop acts on the chunk record in that namespace. The
share keeps `old_refs`, a counter ([RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)) of its refs at *g*, which every
transaction that writes or drops such a ref changes. A clone of a ref by reference
within the re-homing share adopts in the namespace that ref names. A clone or
server-side copy between two shares in different namespaces is refused with
`ErrCrossNamespace` (NFS `NFS4ERR_XDEV`, SMB `STATUS_NOT_SUPPORTED`), as it is
between any two namespaces ([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)), and the client falls back to reading
and writing the bytes.

**Steps.**

1. **Start.** A re-home is refused:
   - with `ErrMoving` during a move of N;
   - with `ErrRehoming` while another re-home of the share runs.

   An unexpired catalog backup of the share does not refuse a re-home, although
   its hints and its recovery import name chunks in N. Its blocks are kept by
   **parking** instead (step 3): the share's refs leave N's live set but stay
   counted in N until the last catalog backup taken before the re-home finished
   has expired. A copying backup needs no parking: it holds its own blocks.

   A share that is N's only share is re-homed like any other; N is left empty
   and is deleted once its GC has collected what the switches retired.

   One transaction then creates N', with a new ID, prefix, key scope and keys and
   its claim written `owned` by this installation. The same transaction adds N' to
   the share's record at *g* + 1 as its **write namespace**, and writes the
   **re-home record** `S‖id‖rh`, holding the cursor and the pass number. Until the
   re-home finishes, the share refuses clones of its snapshots and new catalog
   backups with `ErrRehoming`, and a move of N or of N' is refused with
   `ErrRehoming` too. Snapshots and copying backups go on ([RFC 26 §2.4](rfc-26-catalog-backups.md#2.4%20Copying%20backups)). A clone or
   catalog backup that is already running finishes first.
2. **Switch writes.** Each primary of the share's shards is told of the change. It
   carves every new offer under N', and adopts — once deduplication is added —
   only from N'. Chunk IDs are fixed when an offer is carved, before any put
   attempt, so **an offer captures its namespace and that namespace's keys when it
   is carved** ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope), [RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline)), and every later step of it — the plan,
   the names it mints, the puts, the commit — uses the captured namespace, not
   the write namespace current when that step runs. The primary joins every offer
   captured under N, through its commit or its abandonment, before it
   acknowledges the switch. The put-intent step **MUST** refuse, with
   `ErrScopeMismatch`, a plan whose chunk IDs are keyed under a namespace other
   than the one it mints names into, so an offer carved under N can never put
   N-keyed IDs into N'. From then on, offload commits write refs at *g* + 1.
3. **Copy and switch, in batches.** The re-home walks the share's ref keys, live
   and history, in key order from its cursor. It takes refs at *g* per batch, as
   many as the key budget K allows ([RFC 6 §5.2](rfc-6-block-metadata.md#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed)).
   - **Copy.** Chunk IDs are keyed per namespace ([RFC 2 §4.1](rfc-2-carver.md#4.1%20A%20chunk)), so a chunk
     has one ID in N and another in N'. For each chunk the batch names, the
     re-home reads it from N — a ranged get of its body
     ([RFC 4 §4.4](rfc-4-remote-tier.md#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)), decoded and checked against its N ID ([RFC 5 §2.6](rfc-5-transforms.md#2.6%20The%20plaintext%20hash%20is%20the%20final%20check)) — and
     computes its N' ID. Where N' already has a live chunk record for that ID,
     nothing is put. Where N''s chunking settings differ from N's, or a chunk
     exceeds N''s chunk maximum, the re-home reads the bytes each ref covers
     and cuts them under N''s settings, and each old ref becomes the refs of its
     pieces. The chunks go to N''s block assembler, which puts new blocks under
     N''s scope and chain. Each put has a put intent
     and runs through a background flow of the syncer. This is relocation's read,
     mint and put ([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)), with the target in another namespace.
   - **Switch.** Once those puts have succeeded, one transaction:
     - consumes their intents and creates their block and chunk records in N';
     - reads each ref of the batch with conflict tracking, and requires it
       unchanged;
     - adopts each ref's chunk in N', by its N' ID, conditional on existence and
       resurrecting a retired block ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence), [RFC 9 §3.3](rfc-9-gc.md#3.3%20Adoption%20resurrects%20a%20retired%20block)) — normative here
       whatever is deferred with deduplication — and writes its reverse key there;
     - drops the ref from N: it decrements the chunk, removes the reverse key, and
       retires any block it leaves at zero ([RFC 9 §2.2](rfc-9-gc.md#2.2%20Retirement%20is%20decided%20where%20the%20count%20reaches%20zero)) — or, while an
       unexpired catalog backup of the share taken before the re-home finished
       exists, **parks** it: the ref moves to N's own parked prefix
       (`NS‖N‖pk‖…`, [RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), keyed by the share, keeping its count
       and reverse key, where no read, snapshot or listing sees it and only N's
       GC counts it. It is filed under N, not under the share's prefix, and owned
       by N's installation, so moving the share out later carries none of it and
       leaves none of N's counts behind;
     - rewrites each ref at *g* + 1, naming its N' chunk ID, with the same extent,
       version, `born` and `died`; a ref that was re-cut becomes its pieces,
       each with those same values;
     - lowers `old_refs` and advances the cursor.

     A ref that changed since it was read makes the batch retry.
4. **Catch-up passes.** Refs at *g* can appear behind the cursor. A server-side
   copy by reference from another share of N is one source. A version moved to
   history under a key the cursor already passed is another. When a pass ends with
   `old_refs` above zero, the next pass scans only the share's records whose change
   sequence ([§2.2](#2.2%20The%20move%2C%20step%20by%20step)) is above the previous pass's start, and switches what it
   finds.
5. **Close, then finish.** When `old_refs` reads near zero after a pass, one
   transaction marks the re-home `closing`. From then on nothing can raise
   `old_refs`: a server-side copy by reference from N into the share copies
   bytes instead, as between any two namespaces, and every other transaction that touches a
   ref at *g* only moves it to history, which keeps its count, or drops it,
   which lowers it. The re-home then runs catch-up passes until a fold of the
   counter ([RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)) reads zero. Since the counter can only fall
   once `closing` is set, a folded zero stays zero, so the finish transaction
   reads the folded record and the `closing` flag — two keys, never the
   counter's unfolded deltas — makes N' the share's only namespace, deletes the
   re-home record, and lifts the refusals. A finish that had to read every
   unfolded delta would scan a range every writer of the share appends to, and
   conflict with each of them.

N's GC reclaims each chunk the share alone held, as its count reaches zero. Chunks
the other shares hold stay in N at their counts. Nothing in N is deleted on the
re-home's behalf.

**Parked refs outlive the backups that need them.** When the last catalog
backup of the share taken before the re-home finished expires, the parked refs
are dropped from N in batches ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)), decrementing each chunk, and N
can then empty. Until then a recovery import of such a backup resolves its hints
in N as it would before the re-home, and finds every block they name. N is not
deleted while it holds parked refs; a move of N carries them, being under N's
prefix, and a move of the re-homed share carries none. The parking
costs the space of the share's content in both namespaces for one catalog
retention.

**The switch is not a new version.** It rewrites which namespace counts a ref's
chunk, and nothing a snapshot reads. So it passes no cut gate, stamps nothing and
writes no history. Every snapshot and every live read returns the same bytes
before and after it. Snapshots of the share keep working throughout: every ref,
at either generation, counts its chunk in the namespace it names. Cuts, holds and
deletions run as usual. A deletion that drops a ref drops it at whichever
generation that ref names.

**Backups.** A re-home of a large share runs for weeks at its paced rate, and the
share's backups do not stop for it. Copying backups go on throughout: the copier
resolves each ref's chunk in the namespace its generation names, and copies that
namespace's block, whichever it is, into **N′'s folder** ([RFC 26 §2.4.1](rfc-26-catalog-backups.md#2.4.1%20Layout%20at%20the%20location)), under N′'s
folder lease alone. So a backup taken mid-re-home lists blocks of one folder,
each with the namespace that sealed it, and its census lists both namespaces'
material. **Its export carries both namespaces**: both IDs, both sets of key
records with the master-key IDs that wrap them, and both scopes, each chunk
record naming its namespace; it is filed under N′, and its state object names
N′'s folder only. N's sweep never sees it and needs not: no block it lists is
in N's folder, so a listing of one folder finds every backup its sweep must
honour. Its base is matched in N′'s folder, so an N block is copied there once
and reused by later copies until no ref names it. A restore from it decodes each chunk with
the material of the namespace it came from ([RFC 26 §2.4.5](rfc-26-catalog-backups.md#2.4.5%20Restore%20into%20a%20new%20namespace)). Catalog backups,
which hold blocks only through the namespace's counts, are refused while the
re-home runs and resume at its end naming N'. A policy that takes catalog
backups skips them during a re-home and counts each skip; a share that needs
backups through a long re-home is given a copying policy first.

**Progress, failure and pacing.**

- **The cursor is durable.** The re-home record keeps the cursor and the pass,
  advanced by each switch transaction. A restart resumes at the cursor.
- **A crash between the puts and the switch** leaves blocks in N' named only by
  their intents. N''s GC collects them ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)), and the retried batch puts
  again.
- **Pacing.** The copy runs in the syncer's background class, under
  `rehome.rate`. A rate of 0 pauses it.
- **Cost.** Each byte the share references is read once and written once. There
  is one ranged get per chunk, and two chunk-record updates and one ref rewrite
  per ref.

> decision: a re-home cannot be cancelled once it has started, only paused. Half
> done, the share's content is split across two namespaces, and both count it
> correctly. Stopping there leaves the share serving correctly, and finishing is
> the only way back to one namespace. A reverse re-home into N would bring
> deduplication back, but it is not specified. Add it if operators start re-homes
> they regret.

> ponytail: a re-home reads each chunk with its own ranged get, even when most
> of a block's chunks belong to the share. Upgrade to one whole-block get when
> most of a block is named, as restore does ([RFC 26 §2.4.5](rfc-26-catalog-backups.md#2.4.5%20Restore%20into%20a%20new%20namespace)), when re-home request
> counts show in its cost.

**Worked example: re-homing one share while it serves** — `S-snap-rehome`. Shares
`vm1` and `vm2` share namespace `ns-vms`. `vm2`'s file `f` has live refs r1 → c1,
which `vm1` also holds, and r2 → c2. It also has a history ref h3 → c3, which
snapshot 4 of `vm2` sees.

| t | action | `ns-vms` counts | `ns-vm2` | `old_refs` |
| --- | --- | --- | --- | --- |
| 0 | start: `ns-vm2` created, generation 2 added to `vm2`; clones and backups of `vm2` refused | c1 2, c2 1, c3 1 | empty | 3 |
| 1 | `vm2`'s primary joins its attempts under `ns-vms` and acknowledges | — | — | 3 |
| 2 | a client writes `f`; its offload carves c4 into `ns-vm2`, ref r4 at generation 2 | — | c4 1 | 3 |
| 3 | batch 1: ranged gets of c1, c2, c3 from `ns-vms`; b9 put in `ns-vm2` under an intent | — | b9 stored, unrecorded | 3 |
| 4 | switch: b9 recorded; r1, r2, h3 adopt in `ns-vm2`, drop from `ns-vms`, rewritten at generation 2 with their `born` and `died` | c1 1, c2 0, c3 0: their blocks retire if nothing else is live in them | c1, c2, c3, c4 1 | 0 |
| 5 | `vm1` server-side copies a file into `vm2` by reference: ref r5 → c5 at generation 1 | c5 +1 | — | 1 |
| 6 | pass 2 scans records above pass 1's change sequence and switches r5 | c5 −1 | c5 1 | 0 |
| 7 | finish: `ns-vm2` is `vm2`'s only namespace | — | — | — |
| 8 | a read of snapshot 4 resolves h3 at generation 2 in `ns-vm2` | — | — | — |

Chunk names in the `ns-vm2` column stand for the same bytes under `ns-vm2`'s
own chunk IDs. `vm1` still reads c1 from `ns-vms`. `vm2` can now be moved to
another installation alone.

## 3. API surface

### 3.1 Interfaces

Signatures are indicative; the obligations above are normative.

```go
// Rehome copies one share into a new namespace of its own (§2.7).
type Rehome interface {
	Start(ctx context.Context, share ShareID, ns NamespaceSpec) error
	Status(ctx context.Context, share ShareID) (RehomeStatus, error) // pass, cursor, old_refs, bytes copied
	SetRate(ctx context.Context, share ShareID, bytesPerSec int64) error // 0 pauses; there is no cancel
}

// Migration moves a namespace (§2.2).
type Migration interface {
	PreSeed(ctx context.Context, ns NamespaceID, w io.Writer) (Digest, error)          // steps 1–2
	Freeze(ctx context.Context, ns NamespaceID, base Digest, w io.Writer) (Digest, error) // steps 3–7
	Import(ctx context.Context, r io.Reader) (ImportReport, error)                     // base, then delta
	Drop(ctx context.Context, ns NamespaceID, imported Digest) error                   // step 9
	Abort(ctx context.Context, ns NamespaceID, notPublished bool) error                // after step 7 only with notPublished
}
```

Errors join RFC 12's closed set: `ErrMoving`, `ErrFreezeTimeout`,
`ErrNotClaimed`, `ErrFileIDExists` and `ErrRehoming`; an import also returns
[RFC 26 §4.1](rfc-26-catalog-backups.md#4.1%20Interfaces)'s.

What other components gain:

- **GC** ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)): the namespace's pause record, read before every pass
  and batch, and by every relocation commit with conflict tracking.
- **Re-homes** ([§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)): a share's namespaces by generation, and each ref's
  generation, which reads, adoptions, drops and the dedup oracle resolve in; the
  `old_refs` counter; the re-home record; a write namespace that each offer
  captures, with its keys, when it is carved, and an intent step that refuses a
  plan keyed under another namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)); the share's parked refs
  under the old namespace's own prefix.
- **The journal at a move** ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)): the drop calls `Forget` on the moved
  shares' tags in every journal; attaching an imported share's tag `Forget`s,
  or refuses, any extent of it the journal still holds.

### 3.2 Configuration

Moves and re-homes are control-plane records, set through the API
([RFC 13 §2.1](rfc-13-configuration.md#2.1%20The%20control%20plane%20is%20the%20source)) like every other record, with the scope and
class [RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings) gives each: `migration.*` per installation, and
a re-home's rate. The shape below is how a provisioning file declares them
([RFC 13 §2.4](rfc-13-configuration.md#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).

```yaml
migration:
  freeze_timeout: 5m            # bound on a move's freeze, through B's ready (§2.2)
  hold_reply: 1s                # longest an NFS call waits in the freeze before ErrDelay; SMB is held pending
                                # up to freeze_timeout + 35 s, as for ErrGrace (§2.2, RFC 17 §3.2)
rehome:
  rate: 100MiB/s                # a re-home's copy rate; 0 pauses it (§2.7)
```

## 4. Invariants

Invariant numbers are shared by RFC 12, RFC 26 and RFC 27, and each invariant is
stated in the one that owns its subject; the numbers missing here are theirs.

| # | Invariant |
| --- | --- |
| S12 | One installation holds a namespace's claim. Only it puts, sweeps, relocates or collects there, only its store holds the namespace's records, and it writes only while the claim names it `owned` and its record says `serving`. A namespace being moved has no relocation, delete or collection from the pre-seed on, across restarts. |
| S13 | A move never releases refs at the old installation and never deletes a remote object on its behalf. |
| S17 | A move keeps FileIDs and refuses a collision; a clone, a restore and a detached snapshot get new ones. A move's drop `Forget`s the namespace's shares' tags in every journal at the old installation — every record of each tag below the forget record's sequence number, by sequence number, not by version — leaving no extent or removal marker of them, and attaching an imported share's tag finds none of its extents. |
| S20 | After a move, the base plus the delta equal the source's records at the freeze; the base is exported only once its cuts are `complete`, and the delta is every record whose change sequence is above the base's. |
| S26 | During a re-home every ref of the share names the namespace its chunk is counted in, by that namespace's chunk ID. A switch adopts in the new namespace and drops from the old in one transaction, keeping extent, version, `born` and `died`, so no snapshot's or live read's bytes change. The re-home finishes only when no ref names the old namespace, decided from a folded counter that cannot rise once the re-home is closing. |
| S32 | A claim carries the holder's instance nonce; a holder reads the claim before it rewrites it and records the new nonce before it puts it; a process that reads its own identity with a nonce it did not record stops writing and deleting at once, and an installation started on a copy holds no namespace until an operator states which copy it is. |
| S38 | An offer uses the namespace and keys it captured when carved; no put lands N-keyed chunk IDs in another namespace. A copy taken mid-re-home carries both namespaces, puts every block into the new namespace's folder, and is honoured by that folder's sweep; a ref the re-home drops from N stays counted there while a catalog backup taken before the re-home finished is unexpired. |
| S39 | After a move's drop, no journal of the old installation holds an extent or removal marker of the moved shares, and a journal attaching an imported share holds none of its tag's extents before it serves. B's work after A's release is one publish and the claim. |

## 5. Test plan and benchmarks

The set-wide rules and tiers are in [the RFC index](rfc-index.md#Test%20tiers).
Every check runs against a real metadata backend and the remote-tier emulator; GC
runs as in production, not stubbed.

### 5.1 How it is tested

These checks run on [RFC 12 §5.1](rfc-12-snapshots.md#5.1%20How%20it%20is%20tested)'s
model checker, deterministic simulator and model-based run.

**A model.** The move's claim, pause and freeze ([§2.1](#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim),
[§2.2](#2.2%20The%20move%2C%20step%20by%20step)) and the re-home's switch and finish
([§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) **MUST** be modelled
in a model checker before they are implemented, with S12, S13, S20 and S26 as
properties, and kept in step with this document.

**Coverage.** Seed search **MUST** also reach each of these or fail: a stale GC
process stopped by its claim fence; a relocation aborted by the GC pause record;
a re-home batch retried on a changed ref; a catch-up pass switching a ref written
behind the cursor; a parked ref dropped when the last pre-re-home catalog backup
expired; a claim read that stopped a stale image before its put.

### 5.2 Scenario catalogue

| ID | Scenario |
| --- | --- |
| `S-snap-move-preseed` | [§2.2](#2.2%20The%20move%2C%20step%20by%20step)'s example: a pre-seeded move with a short freeze |
| `S-snap-move-crash-<step>` | A or B crashes after each of [§2.2](#2.2%20The%20move%2C%20step%20by%20step)'s steps; a restart during `moving` stays closed |
| `S-snap-move-stale-gc` | A is partitioned, not dead, during a recovery import; its GC stops on its claim fence |
| `S-snap-move-delta-seq` | during the pre-seed an offload lands late with `born` below the base, a truncate narrows a ref keeping its `born`, and a count reaches zero and retires a block; the delta carries all three by change sequence and B reads back A's bytes |
| `S-snap-move-gc-pause` | a relocation is in flight when the pause record is written; it aborts, A restarts during the pre-seed, and no relocation, delete or collection runs until B publishes |
| `S-snap-move-refuses` | a clone, a backup, a share creation and a share deletion requested during a move are refused `ErrMoving`; one already running finishes first |
| `S-snap-rehome` | [§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)'s example: a re-home under writes, a cross-share copy caught by a second pass, and snapshot reads before and after |
| `S-snap-rehome-crash-<step>` | the re-home crashes after each step and between a batch's puts and its switch; it resumes at its cursor, and N''s GC collects the orphaned puts |
| `S-snap-rehome-straddle` | an offload carved under N mints names and puts after the write namespace changed: it puts into N with N-keyed IDs, the primary's join makes it commit before the acknowledgement, and every read of it verifies; hand the intent step a plan keyed under N for a put into N': refused `ErrScopeMismatch` |
| `S-snap-rehome-sole` | the only share of its namespace is re-homed into a namespace with a different chunking target and encryption on; it serves throughout, every snapshot reads back, its refs are re-cut, and the old namespace ends empty |
| `S-snap-rehome-backup` | copying backups run every day of a long re-home; each lists blocks of N′'s folder only, sealed in either namespace, and carries both namespaces' IDs and key records, a restore from one taken mid-way reads back, and N′'s sweep keeps every block it lists while N's sweep deletes none of N′'s; new catalog backups are refused and counted. A share with a 90-day catalog backup is re-homed at once: its refs are parked in N, a recovery import of that backup reads back after the re-home finished, and N empties only once the backup expires |
| `S-snap-rehome-finish-load` | the finish runs while 64 writers write the share: it commits on the folded counter alone, without a range read; a server-side copy by reference from N after `closing` copies bytes |
| `S-snap-cloned-vm` | two copies of one installation start from one disk image |
| `S-snap-move-freeze-reply` | calls during a move's freeze are answered retry-later after `hold_reply` over NFS, kept pending over SMB within `freeze_timeout` plus 35 s, never answered `STATUS_DISK_FULL`, the SMB connection dropped at the move so durable handles reconnect at B, and all complete at B |

### 5.3 Group A — lost or wrong content

| Invariant | Check |
| --- | --- |
| S12 | Two installations on one bucket, each holding one namespace, both running GC, relocation and collection for a day-tier run: neither deletes an object the other's records name. Configure both to hold one namespace: GC stops on the claim check. `S-snap-move-stale-gc`. |
| S13 | Move a namespace while A's GC has retired blocks in the trash, deleted blocks awaiting their delete, and intents in flight. After the drop, A issues no delete; B resumes them, and deletes no retired block before its `not_before`. |
| S17 | Move a namespace to B, back to A, and to B again: FileIDs preserved. Restore one backup twice: two shares, disjoint FileIDs. Move A→B, overwrite a file at B, move back: A reads B's bytes, and A's journals held no extent of the share between the drop and the return. Skip step 9's `Forget`: A serves its stale extent. Give an old record of the tag a version above the forget's: it is still dropped, since `Forget` covers by sequence number. Replace it with a per-file `Delete`: after A→B→A a restart's `Since` yields the markers and an untouched file loses its content. Crash A between the `Forget` calls and the record drop, and plant a stale extent of the tag: the attach `Forget`s it or refuses, and never serves it. |
| S20 | `S-snap-move-preseed` under the model-based run on A during the pre-seed: after the move B's records equal A's at the freeze, and the delta holds no record unchanged since the base. `S-snap-move-delta-seq`: select the delta by `born` and `died`, and B misses the narrowed ref. `S-snap-move-gc-pause`: pause in memory only, and B maps a relocated chunk to its deleted block. |
| S26 | `S-snap-rehome` and `S-snap-rehome-crash-<step>` under the model-based run: every snapshot and the live share read back their model copies at every step, and after the finish no ref names N and the audit of both namespaces is clean. Skip the catch-up passes: finish refuses, since `old_refs` is not zero. |
| S32 | `S-snap-cloned-vm`: start two copies of one installation's disk, on a platform that reports a clone and on one that does not. With the report, the copy holds no namespace. Without it, within one `Recheck` period exactly one copy writes and deletes, and the other has alerted. Restore a day-old image of a running installation, on a platform that reports nothing: it stops at its first start, before any put, and the original keeps writing; rewrite the claim before reading it, and the original is the one fenced. Crash the holder between its put and recording the nonce: at restart it recognises its own claim. |

### 5.4 Group B — cost and wedging

| Concern | Counted check |
| --- | --- |
| [§2.2](#2.2%20The%20move%2C%20step%20by%20step) freeze | The delta exported holds exactly the records changed since the base; the freeze time is reported against the namespace's key count (the scan's ceiling). B's work after A's release is one publish transaction and the claim put, whatever the namespace's size; kill B mid-rebuild, or delay it past `freeze_timeout`: the move aborts and A serves. Rebuild indexes after the release instead: the freeze ends while B still rebuilds for minutes, and an abort is no longer possible. |
| [§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace) re-home | Each chunk is read from N and put into N' once. A re-home with no concurrent writes finishes in one pass. |

### 5.5 Benchmarks and targets

Recorded on the reference box ([the RFC index](rfc-index.md#Test%20tiers)); the
10⁷-file rows run daily.

| Benchmark | Measures | Target |
| --- | --- | --- |
| Move of a 10⁷-file namespace with 1% changed during the pre-seed | freeze to B serving | under `freeze_timeout`; no block transferred |
| Re-home of a 10⁶-file share under 64 writers | MB/s against `rehome.rate`; p99 write latency | within 10% of the rate; p99 within 2× of no re-home |

## 6. Observability

Metric names are shown without the deployment's prefix, which the exporter adds.

| Answers | Metric | Type |
| --- | --- | --- |
| re-home progress per share: refs left at the old generation, bytes copied, passes | `rehome_old_refs`, `rehome_bytes_total`, `rehome_passes_total` | gauge, counter, counter |
| move phase per namespace (`preseed`, `freeze`, `released`, none), freeze duration, and whether the GC pause record is present | `move_phase`, `move_freeze_seconds`, `move_gc_paused` | gauge, histogram, gauge |
| namespace claim state (1 when `owned` by this installation) | `namespace_owned` | gauge |
| claim reads naming this installation with a nonce it did not write, and starts on a copied disk; any value is an alert | `namespace_claim_copy_total` | counter |
| GC passes stopped by the claim check or its fence; any nonzero value is an alert | `gc_claim_refusals_total` | counter |

Logs: each move step and publish logs at `Info` with namespace and export digest,
and each re-home pass with its counts. A move's freeze timeout and a claim
mismatch log at `Warn`, naming the claiming installation.

## 7. Open questions

1. **Reading another installation's snapshot without a move** would count blocks
   the holder can sweep; it needs replication across installations or a byte copy.

---

## Appendix A — where the current code differs

Descriptive, for the refactor.

| # | This document says | The code today |
| --- | --- | --- |
| D9 | Namespaces move between installations, pre-seeded ([§2](#2.%20Moving%20a%20namespace%20between%20installations)) | none; no claim, no export format |
| D12 | A move exports intents and retired and deleted block records ([§2.3](#2.3%20GC%20across%20installations%20on%20one%20bucket)) | none |
| D16 | A share is re-homed into a namespace of its own while it serves ([§2.7](#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) | none; every share has its own store |
