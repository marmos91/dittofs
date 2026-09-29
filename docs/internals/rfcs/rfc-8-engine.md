---
rfc: 8
title: "RFC 8 — the engine"
component: engine
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-9-gc]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
aliases:
  - RFC 8
tags:
  - rfc
---
# RFC 8 — the engine

**Status:** draft.
**Audience:** anyone changing the engine, the per-share composition, or the
content surface adapters call.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists where the code differs, as defects to fix, never
rules to build around. Where it chooses a policy the set left open, the choice is
labelled **proposal** and names the measurement that would overturn it.

---

## In short

- The engine holds no bytes and no facts. Every byte is the journal's or the
  remote tier's; every fact is block metadata's or the namespace's.
- It does three things for one share: it **composes** the components, it
  **decides** policy, and it is the **facade** adapters call for content.
- It owns the joins no component can own alone: residency resolution on read,
  block assembly on offload, and the ordering of a write.
- Policy chooses among safe actions. It is never the thing that makes an action
  safe.
- It persists nothing. Losing all of its state costs performance, never content.

---

## 1. Purpose

[RFC 0 §1.1](rfc-0-data-lifecycle.md#1.1%20The%20component%20set) gives the engine *composition, policy, the facade adapters call*, and
nothing else. It answers the one question no component can:

> **Given what the two oracles say, what happens next?**

A read has two answers to join. An offload has a carver, a syncer and a metadata
commit to sequence. A full journal has to be told what to evict. A write has
steps owned by three components. Each needs someone who sees all the parties and
belongs to none of them.

### 1.1 Non-goals

The engine **MUST NOT**:

- define a format, an encoding or an algorithm ([RFC 1](rfc-1-journal.md), [RFC 2](rfc-2-carver.md), [RFC 4](rfc-4-remote-tier.md), [RFC 5](rfc-5-transforms.md));
- move bytes to or from the remote tier itself — that is the syncer ([RFC 3](rfc-3-syncer.md));
- hold a copy of any oracle's answer that outlives the operation that asked — no
  residency cache, no durability record ([RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function));
- persist anything. Every durable fact is recorded by the component that owns it;
- decide when an inode stops existing ([RFC 7 §4](rfc-7-namespace-metadata.md#4.%20What%20keeps%20an%20inode%20alive)), or schedule GC ([RFC 9](rfc-9-gc.md)), which
  runs once per remote namespace, not per share ([§7.5](#7.5%20GC%20is%20not%20scheduled%20here));
- carry namespace features nothing below the namespace needs, such as a recycle
  bin ([RFC 7 §13](rfc-7-namespace-metadata.md#13.%20Open%20questions)).

## 2. Composition

### 2.1 The engine is the composition root, and the only one

Content is built in one place, from configuration and the backends it names: the
parts several shares share — one **journal per device**, the syncer, block
metadata — are constructed once, and one engine per share is constructed over
them. That construction **MUST** be the only code that names a concrete component
type. Everything else — adapters, the runtime, other components — holds a
declared interface. [RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)'s import rule binds the components composed; the
composition code imports them and nothing imports it.

Composition **MUST** happen at construction. A capability **MUST NOT** be wired
onto a serving engine by a setter: that makes "this capability is absent" a
reachable state of a live share. Changing a share's backends is a new engine.

The engine supplies, at construction:

| Declared by | Need | Supplied from |
| --- | --- | --- |
| [RFC 3](rfc-3-syncer.md) | a `Store`: streamed put, verified read, health ([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)) | the block codec with the store's transform chain ([RFC 4 §3](rfc-4-remote-tier.md#3.%20The%20block%20format), [RFC 5](rfc-5-transforms.md)), over the remote block store |
| [RFC 7](rfc-7-namespace-metadata.md) | `Size`, `Times`, allocation, `Release` ([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)) | block metadata's existence records with the journal's uncommitted operations applied ([§4.1](#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not)); the facade's `Release` ([§9.1](#9.1%20One%20facade%2C%20shaped%20like%20content)) |

Every capability **MUST** be a method on an interface held by declaration, and an
absent one **MUST** fail the build or construction with an error naming it
([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)). A production constructor that accepts `nil` for a capability "so
tests can omit it" has made a silent fallback a production path; fixtures supply
stubs of the whole interface.

### 2.2 Capabilities are parameters, never assertions

The engine **MUST NOT** negotiate a capability by type assertion, nor fall back to
a degraded behaviour when one is missing ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)). Here each such fallback
yields a working share and no error: an assertion failing on the remote tier
disables offload, on a transform chain uploads plaintext, on a lookup makes every
cold read linear.

### 2.3 A share is one engine

Each share has exactly one engine and one assembly of policy state. Engines of
shares on one device share that device's journal, which accounts capacity per
share and keeps one share from starving another ([RFC 1 §7](rfc-1-journal.md#7.%20Capacity)). Backends below the
engine — a remote store, a metadata store — **MAY** be shared between shares.

The engine **MUST** refuse a composition in which two block-metadata stores can
name one remote key ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)); [§5.4](#5.4%20A%20block%27s%20name%20is%20minted%20here%2C%20and%20its%20intent%20recorded%20before%20the%20put) is how it avoids that.

### 2.4 Settings are validated once, and refused rather than replaced

The engine validates every setting it composes at construction, and **MUST**
refuse an invalid one with an error rather than substitute a default
([RFC 2 §3.7](rfc-2-carver.md#3.7%20Bad%20settings%20must%20be%20refused%2C%20not%20replaced)). It **MUST** record which chunking settings produced a share's content,
and **MUST** report a change to them as a migration rather than apply it
([RFC 2 §3.6](rfc-2-carver.md#3.6%20Changing%20any%20of%20this%20is%20a%20migration)).

### 2.5 Start in order, stop in reverse, and join before closing

**Start.**

1. Read the version floor for every share the device journal may serve
   ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)), and open the journal with it ([RFC 1 §9.1](rfc-1-journal.md#9.1%20Rebuilding)). The journal
   recovers its index and its offloaded-bit ledger alone.
2. For each file the journal lists ([RFC 1 §3.7](rfc-1-journal.md#3.7%20State%20introspection)), read the operations above
   the shape's `applied` version with `Since(id, applied)` — held extents and
   removal markers, each with its version ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Settle%20and%20Since)) — and apply
   them to existence in version order: writes that were acknowledged but never
   reached a stability point, and removals whose metadata step a crash cut off
   ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)). `Since` itself needs no `Sync`; the existence commit that applies
   what it yields syncs the file first, like every existence commit
   ([§9.4](#9.4%20Commit%20is%20answered%20by%20the%20journal)). Once that commits, call `Settle(id, applied)` with the new
   `applied`, which lets the journal drop the markers it covers. No file is served
   before this.
3. Resume every removal not yet done, batch by batch, and every unfinished clone
   ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation), [§9.3](#9.3%20Clone%20adopts%20refs%2C%20and%20offloads%20uncarved%20content%20first)); a clone's destination range is not served until its
   clone is done. Then prune every done removal: no pass is in flight.
4. Start the background loops. Eviction may run at once: the ledger restored the
   offloaded bits ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)). A background **consistency check** compares
   them with the refs ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)) through `MarkDurable`, and reports and clears
   any bit no ref justifies. It is a check, not a gate.

**Restart re-offers everything without an offloaded bit.** Offload plans and
unknown-outcome state live in memory only ([§5.1](#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)). After a restart, every held
extent without an offloaded bit is offered again:

- a re-offer is a new attempt and mints a new name ([§5.4](#5.4%20A%20block%27s%20name%20is%20minted%20here%2C%20and%20its%20intent%20recorded%20before%20the%20put)). Chunks a
  block committed before the crash are found by the dedup oracle and adopted;
  the rest are carried again;
- a block that was put and never committed is named by an intent under the
  previous epoch and by no block record. Its name is never used again, and the
  object is an **orphan: leaked space, not lost content**, which collection
  reclaims from its intent ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)).

> [!important] Pending review — recovery through `Since` and `Settle`
> Recovery reads operations above `applied` with the journal's `Since` and
> settles after the existence commit. Removals and clones resume from their
> durable records. A restart re-offers under new names instead of re-deriving one.
> `Since` needs no `Sync`; the commit applying it syncs first, as all existence
> commits now do. Outside recovery, each removal settles after its phase 1.

**A new owner settles before serving.** When a share's files change owner —
takeover or handover ([RFC 11](rfc-11-ownership.md)) — the new owner settles each file to the sealed or
drained point ([RFC 10](rfc-10-journal-replication.md)) and runs step 2 for it before serving it.

**Stop.** Stop accepting operations. Cancel the background loops, then join them.
A component **MUST NOT** be closed while any work that uses it is still running
([RFC 1 §10.7](rfc-1-journal.md#10.7%20Shutdown)). A join that does not complete within its bound **MUST** leave the
components it depends on open and report the failure, rather than close them
under a live loop.

## 3. Policy

### 3.1 Policy is decided here and executed below

| Decision | The engine decides | The mechanism belongs to |
| --- | --- | --- |
| When to offload a file | eligibility and urgency ([§4.2](#4.2%20Offload%20is%20scheduled%20here)) | journal `Offload` ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload)) |
| What goes in a block | assembly ([§5](#5.%20Block%20assembly)) | carver, for chunks ([RFC 2](rfc-2-carver.md)) |
| When to put a block | as soon as it is assembled | syncer uploader ([RFC 3 §3.1](rfc-3-syncer.md#3.1%20It%20is%20triggered%2C%20not%20scheduled)) |
| Whether to fill | [§6.3](#6.3%20Filling%20is%20a%20decision) | journal `Fill` ([RFC 1 §3.4](rfc-1-journal.md#3.4%20Fill)) |
| What to read ahead, pre-warm | [§6.4](#6.4%20Speculation%20is%20planned%20here%2C%20and%20yields%20to%20demand%20and%20to%20writes) | syncer fetcher ([RFC 3 §4.5](rfc-3-syncer.md#4.5%20Speculation%20is%20executed%20here%20and%20decided%20elsewhere)) |
| What to evict, and when | [§7.1](#7.1%20Eviction%20is%20chosen%20here%2C%20and%20needs%20no%20new%20record) | journal `Release` ([RFC 1 §3.5](rfc-1-journal.md#3.5%20Release)) |
| When to repack | [§7.3](#7.3%20Repack%20is%20triggered%20here) | journal repack ([RFC 1 §8.2](rfc-1-journal.md#8.2%20Repack)) |
| Whether to keep accepting writes | [§7.2](#7.2%20A%20capacity%20refusal%20comes%20back%20here) | journal capacity ([RFC 1 §7](rfc-1-journal.md#7.%20Capacity)) |
| What follows from ill health | [§8.1](#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included) | — |

A threshold that lives in a component's configuration is engine policy absorbed
by that component.

### 3.2 Policy never makes an action safe

Every mechanism the engine calls is safe by its own definition: `Release` refuses
an extent whose offloaded bit is unset ([RFC 1 §3.5](rfc-1-journal.md#3.5%20Release)), `Fill` refuses to overwrite held
bytes ([RFC 1 §3.4](rfc-1-journal.md#3.4%20Fill)), an offloaded bit is set only on a durability report. A
policy gate **MUST NOT** be the only thing standing between the system and data
loss. The test: disable every gate — evict as eagerly as possible, fill
everything, offload constantly — and the Group A checks still pass ([§12](#12.%20Conformance)).

### 3.3 Policy state is memory, and disposable

Access patterns, dirty ages, offload backoff, readahead frontiers, offload plans
and derived health **MUST** live in memory. A restart **MAY** lose all of it, and
the engine **MUST** behave correctly from an empty policy state.

## 4. The write and the offload

### 4.1 The facade orders a write; adapters do not

A write is one facade operation. The engine performs, in this order:

1. authorise the write against the namespace ([RFC 7 §7](rfc-7-namespace-metadata.md#7.%20Permissions));
2. stage the bytes in the journal, which assigns the write its version
   ([RFC 1 §3.1](rfc-1-journal.md#3.1%20Write)) — and, where replication is composed, hold them durably at every
   member ([RFC 10 §4](rfc-10-journal-replication.md#4.%20The%20write%20path));
3. acknowledge.

Existence — `size`, holes, `mtime`, `ctime` — is committed at the file's next
**stability point**, group-committed with every other file of the journal that
has pending existence ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal), [§9.4](#9.4%20Commit%20is%20answered%20by%20the%20journal)). Until then the journal is the
authority for the write: `Read`, `Size`, `Times` and `Allocation` apply the
journal's uncommitted operations of the file over the committed records. A
failed existence commit fails the stability reply; the operations stay pending
and the next stability point retries them.

No adapter **MAY** perform these steps itself or in another order. A sequence
copied into each protocol handler is a sequence each handler gets wrong
differently.

### 4.2 Offload is scheduled here

A file becomes eligible for an offload pass when its dirty bytes reach the block
target, when its oldest dirty byte reaches a maximum age, or when the journal is
under capacity pressure ([§7.2](#7.2%20A%20capacity%20refusal%20comes%20back%20here)). A client's request for durability is not a
trigger ([§9.4](#9.4%20Commit%20is%20answered%20by%20the%20journal)).

**Proposal:** a dirty-byte threshold of one block target and a maximum age of a
few seconds, with capacity pressure making every file with dirty bytes eligible.
Overturned by a measurement showing the age bound, not the byte bound, fragments
blocks under a streaming workload.

**A pass offers at most `upload_workers` block targets of dirty bytes**, across
the files it covers, through the journal's `limit` ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload)). A large file is
offloaded as a sequence of passes, and small files are gathered into one pass up
to the same bound ([§5.7](#5.7%20A%20block%20packs%20chunks%2C%20whichever%20files%20they%20came%20from)).

A failed pass leaves its extents **Dirty** and is retried with jittered backoff;
retries **MUST NOT** stop, and a failing pass **MUST** be reported to health
([§8.1](#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included)).

### 4.3 The offload guard is narrow

The engine holds a per-file guard **only while capturing an offer and while
committing**, never across the upload. Capturing an offer first commits the
file's pending existence ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)), so no ref is ever written for content
existence does not record. Two passes of one file may therefore upload at once;
block metadata orders their commits by content version ([RFC 6 §4.4](rfc-6-block-metadata.md#4.4%20Commits%20for%20one%20file%20apply%20in%20order)), and the
guard only keeps two commits of one file from conflicting with each other.

`Truncate`, `Deallocate`, `Release` and `Clone` hold the destination file's guard
across their journal step and their removal's first metadata transaction
([§9.1](#9.1%20One%20facade%2C%20shaped%20like%20content)), so no offer is captured between the two; the removal then masks
what its later batches drop. A clone also holds the source's guard until it is
done ([§9.3](#9.3%20Clone%20adopts%20refs%2C%20and%20offloads%20uncarved%20content%20first)). A pass over several files takes their guards in
file-identity order. The guard **SHOULD** be keyed by file: a striped guard
serialises unrelated files that collide on it.

**The guard is an optimisation, not a fence.** It saves commits from
conflicting and retrying; correctness **MUST NOT** depend on it. Two processes
that each believe they own a file hold two guards, and what orders their commits
is the metadata store's conflict on the file's fence records ([RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency)).

Every metadata commit the engine makes for a file — existence, offload, removal —
carries the **owner epoch** of the file's ownership unit, and block metadata
refuses it when the epoch is stale ([RFC 6 §4.1](rfc-6-block-metadata.md#4.1%20What%20one%20commit%20records), [RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency)).

> [!important] Pending review — the guard is not a fence
> Stated because neither metadata backend offers a range lock: the per-file
> fence records are the conflict point, and the in-process guard only saves retries.

### 4.4 Transfers survive removals under them

A truncate, deallocate, release or clone that lands while a pass is uploading
does not wait for it and does not cancel it:

1. the removal takes the guard, which the uploading pass does not hold;
2. the journal removes the range at a new version *v* and keeps the offered bytes
   readable through `offered` until the pass returns ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload));
3. metadata records `Removal(file, v)` ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)) in one small transaction,
   then drops the refs it covers in bounded batches; until it is done it masks
   those refs from every read;
4. the upload completes, and its commit drops every ref whose `newest` is below
   an overlapping removal's version, and applies the rest, whether or not the
   removal's batches have finished;
5. the chunks only the dropped refs used are dead weight in the block, for GC to
   reclaim.

A pass whose offered content was entirely removed **MAY** abort before uploading.
The rule is tested in [§12.1](#12.1%20Group%20A%20%E2%80%94%20lost%20or%20wrong%20content).

After a pass, the file's owner prunes its removal records that are done and at
or below the durable floor ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)).

> [!important] Pending review — batched removals
> A removal is a durable record written first and applied in batches of at most
> K refs, so a large truncate fits the backend's transaction limits.

### 4.5 The callback returns only what committed

The offload callback **MUST** report, through the journal's `report`, exactly the
extents whose commits succeeded, as each block's commit lands, and **MUST NOT**
report any other ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload), [RFC 6 §4.3](rfc-6-block-metadata.md#4.3%20The%20commit%20is%20the%20report%27s%20return%20edge)). A put that succeeded and whose
commit did not is not durable. Order does not matter: one failed block holds back
no other. Refs found already committed are reported like applied ones. A block
that carries chunks of several files makes all of them durable at once, and the
callback reports each file's share then.

### 4.6 A share with no remote tier never reports durability

On a share with no remote tier the offload callback **MUST** return nothing, and
no offloaded bit is ever set ([RFC 6 §4.2](rfc-6-block-metadata.md#4.2%20Only%20after%20durability)). The content stays **Dirty** for its
lifetime, and eviction has nothing to act on — by definition, not by a gate. Such
a share writes no chunk, block or ref records, and clone copies bytes ([§9.3](#9.3%20Clone%20adopts%20refs%2C%20and%20offloads%20uncarved%20content%20first)).

## 5. Block assembly

### 5.1 Blocks are assembled here, as a fold over the carver's output

[RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component) places block assembly with whoever owns the dedup query: the engine.
Assembly is a fold over the chunks the carver emits. For each chunk:

- **all zeros:** record a zero ref — no chunk, no bytes carried, reported as a
  hole ([RFC 6 §3.5](rfc-6-block-metadata.md#3.5%20Operations%20that%20make%20holes));
- **known:** ask the dedup oracle ([§5.3](#5.3%20The%20dedup%20oracle%20never%20sees%20an%20uncommitted%20block)), and record it as adopted if it
  answers;
- **otherwise:** carry it into the pending block.

The fold **MUST** obey [RFC 2](rfc-2-carver.md)'s packing rules: whole chunks only (P1); a block
ends when it reaches its target, overshooting it by at most one chunk, or when it
reaches the chunk-count cap, whichever comes first (P2); a block holds only the
chunks whose bytes it carries (P3); and at most `N` chunks, the format's cap
(P4).

**The short last block.** A pass's last block may fall short of the target. The
engine **MAY** hold it back: it does not put it, reports none of its extents, and
they stay **Dirty** and are carved again by the next pass. It **MUST** put it,
however short, once its oldest chunk's bytes reach the offload maximum age
([§4.2](#4.2%20Offload%20is%20scheduled%20here), [RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component)). So at most one short block is put per pass, and none
waits past the age ceiling. Holding back keeps the assembler per pass: nothing
of the held block survives the pass but the dirty extents themselves.

**A pending block is a plan, not a buffer.** The carver's bytes are borrowed for
the length of `emit` ([RFC 2 §2.2](rfc-2-carver.md#2.2%20The%20bytes%20handed%20to%20%60emit%60%20are%20borrowed)), and the engine **MUST NOT** copy them. A plan
records, per carried chunk, its hash and where its bytes sit in the offered
version: a few hundred bytes per block, whatever its size.

**The upload streams.** The upload's source walks the plan and reads each chunk
from the journal's `offered` reader ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload)), and the store puts the block as
a stream — header, then bodies — with one chunk in memory per worker
([RFC 3 §3.4](rfc-3-syncer.md#3.4%20One%20put%20per%20block)). When a transform's output length is not known in advance, or the
service verifies a checksum sent up front, the store walks the plan twice: once to
measure, once to send ([RFC 5](rfc-5-transforms.md)). The offered bytes stay stable for both walks,
which is why the upload runs inside the `Offload` callback that offered them.

The source **MUST** recompute each chunk's hash as it reads and fail the transfer
on a mismatch ([RFC 3 §3.5](rfc-3-syncer.md#3.5%20The%20bytes%20are%20stable%20for%20the%20duration)).

**Before a pass, the engine checks `Healthy(Put)`** on the share's flow
([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)) and skips carving and packing while the store is put-unhealthy.
**Each upload carries a `ctx` whose deadline bounds its queue wait**
([RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows)): the syncer's own ceiling is minutes, and an upload that waits
past the engine's bound fails its pass, whose extents stay **Dirty** for the next.
**Proposal:** the bound is the offload maximum age ([§4.2](#4.2%20Offload%20is%20scheduled%20here)). Overturned by a
queue-wait distribution showing passes failing on it while the store is healthy.

**A plan whose upload ended in an unknown outcome is dropped with its pass.** Its
extents were not reported and stay **Dirty**; the next pass offers them again as
a new attempt, which mints a new name ([§5.4](#5.4%20A%20block%27s%20name%20is%20minted%20here%2C%20and%20its%20intent%20recorded%20before%20the%20put)). Retries of the same put
within the attempt are the syncer's ([RFC 3 §2.5](rfc-3-syncer.md#2.5%20An%20unknown%20outcome%20is%20not%20a%20success)). The dropped name is never
put or committed again: its intent stays until its epoch is superseded, and an
object the put may have left is an orphan collected with it
([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)).

> [!important] Pending review — unknown outcomes, health gate, short last block
> A plan with an unknown outcome is no longer kept and re-offered unchanged: a
> re-offer is a new attempt and mints a new name. Passes check `Healthy(Put)`,
> uploads carry a deadline bounding their queue wait, and the fold states P4 and
> the ceiling on a held-back short last block.

An assembler is per pass and **MUST NOT** survive it.

### 5.2 The target counts carried bytes

P2's target is measured in the bytes a block carries. Adopted chunks and zero refs
contribute nothing, or a mostly-deduplicated file yields blocks of a few
kilobytes.

### 5.3 The dedup oracle never sees an uncommitted block

The oracle is [RFC 6 §8.2](rfc-6-block-metadata.md#8.2%20Deduplication%20lookup)'s `Durable(hash)`. It **MUST NOT** answer from anything
that knows about a block not yet committed — the pending block, a block in
flight, a put whose commit has not landed.

- **Within one pending block**, a repeated chunk **MAY** be carried once and
  referenced twice: both refs and the bytes commit in one transaction.
- **Across blocks in flight**, a repeated chunk **MUST** be carried again: the
  earlier block may fail after the later one commits. The first to commit owns
  the chunk record, and the other adopts it ([RFC 6 §4.1](rfc-6-block-metadata.md#4.1%20What%20one%20commit%20records)).

The oracle's answer is advisory. A chunk it reports may be retired before the
adopting commit applies; that commit then fails ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)), and the engine
**MUST** re-offer the run with the chunk carried. The engine **MUST NOT** hold a
lock, a reservation or an in-process guard to make the answer binding.

### 5.4 A block's name is minted here, and its intent recorded before the put

The engine mints the name once per put attempt ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)): a hash, under a
domain distinct from chunk hashing, of the key scope, a fresh nonce of at least
128 bits, the chain ID ([RFC 5](rfc-5-transforms.md)) and the block's chunk hashes in order, as the
header records them — sealed when the namespace encrypts
([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)). The nonce is
written into the block header, so a whole-block read still recomputes and
verifies the name. The name is final before framing begins. A retry within the
attempt reuses it, its plan and its bytes; nothing else ever puts that name.

Before the put, the engine durably records a **put intent** for the name,
carrying the file's owner epoch ([RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)). The commit that creates
the block record deletes the intent in the same transaction, and fails if it is
absent: GC removed it because the epoch was superseded, and the engine re-offers
the content. Nothing but the intent precedes the put, and no block is skipped as
already stored: two passes that
carry the same chunks put two objects, and the second to commit adopts every
chunk and is born dead ([RFC 9 §3.5](rfc-9-gc.md#3.5%20Finding%20candidates%20costs%20what%20is%20retirable)). Chunk dedup through the oracle
([§5.3](#5.3%20The%20dedup%20oracle%20never%20sees%20an%20uncommitted%20block)) is unchanged.

> [!important] Pending review — names minted per attempt, intent before put
> Replaces the derived name, the pre-put check and the deletion fence. A name is
> never put twice, so a delete can never land under a later put of it. The cost
> is one extra put and one sweep when two passes race on the same chunks.

**Proposal — the key scope is the identity of the block-metadata store that
counts the block.** Two stores can then never name one object ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)),
and a remote store can be shared between shares whose metadata is separate. The
cost is dedup across such shares. Overturned by a deployment that wants
cross-share dedup *and* accepts one metadata store per remote namespace.

### 5.5 Assembly is sequential, and may change without migration

**Proposal:** chunks are assembled into blocks in file-offset order, which keeps a
sequential read's chunks in few blocks. Randomised assembly ([RFC 2 §6](rfc-2-carver.md#6.%20Boundaries%20are%20public)) blurs
the chunk-size fingerprint and, because refs name hashes, not blocks, can be
adopted later with no migration ([§13](#13.%20Open%20questions)).

### 5.6 A run is what the journal offers, widened only to re-tile

The engine offers each maximal dirty stretch the journal holds as one carver call,
and never joins two stretches ([RFC 2 §2.1](rfc-2-carver.md#2.1%20One%20unbroken%20stretch%20per%20call)). It **MAY** widen a run over
contiguous held bytes that are already durable, **only** so the new chunking
re-tiles a ref the run partially replaces. It widens by passing `widen` to
`Offload`, which offers those durable neighbours with the run ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload)): the
journal offers them frozen, like the dirty bytes, and counts them in the offer's
`Oldest`, so a
release or repack cannot pull them from under the pass. The engine **MUST NOT**
read durable neighbours outside an offer.

**The engine tells `Cut` how each stretch ends** ([RFC 2 §2.4](rfc-2-carver.md#2.4%20An%20artificial%20end%20leaves%20the%20tail%20uncut)), through
`realEnd`. The end is real at a hole, at the file's settled end or at a durable
neighbour; it is artificial where the offer's `limit` or age cut the stretch
short. On an artificial end `Cut` returns `consumed`, the bytes it cut; the
engine reports nothing past `consumed`, and the tail stays **Dirty** and is
offered again from there. Once the tail's oldest byte reaches the offload maximum
age ([§4.2](#4.2%20Offload%20is%20scheduled%20here)), the engine passes the end as real, forcing the final cut, so no
tail waits forever.

> [!important] Pending review — widening reads frozen neighbours, stretch ends
> Widening previously read durable bytes the journal had not offered, which a
> concurrent release could free mid-pass. The journal now offers them on request.
> The engine passes `realEnd` to `Cut`, re-offers an artificial tail from
> `consumed`, and forces the cut at the age ceiling.

### 5.7 A block packs chunks, whichever files they came from

A block is a sequence of chunks, and nothing in its definition is per file
([RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component)). A pass covers several files through the journal's `OffloadMany`
([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload)), and cuts blocks from the stream of their chunks — each file's
chunks in offset order, one file after another — by P1 to P3 alone. One block may
hold the tail of a large file, several small files whole, and the head of another.
Object stores are slow on small objects; one block per small file would turn a
hundred thousand small files into a hundred thousand puts.

- **Only within one share**: a block never mixes two flows or two key scopes.
- **A file's chunks within a block are contiguous**, so a packed small file is
  one ranged read ([§6.8](#6.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block)).
- **One commit per block** records the refs of every file in it
  ([RFC 6 §4.1](rfc-6-block-metadata.md#4.1%20What%20one%20commit%20records)).

Packing costs no read amplification, because a cold read asks for its chunks by
range. It costs relocation work when short-lived small files leave blocks mostly
dead ([RFC 9 §4.1](rfc-9-gc.md#4.1%20A%20block%20that%20is%20mostly%20dead%20pins%20its%20dead%20bytes)).

## 6. The read

### 6.1 Resolution is a join, computed per request

![One read: held bytes from the journal, each missing extent classified by metadata as hole, uncarved or carved, the carved ones fetched, the reply taken from the verified bytes, and the fill as a separate dashed decision](img/rfc6-read-resolution.svg)

For a read of `(file, off, len)`, the engine:

1. asks the journal, and receives the bytes it holds and the exact extents it does
   not ([RFC 1 §3.2](rfc-1-journal.md#3.2%20Read));
2. for each missing extent, asks block metadata which class covers it
   ([RFC 6 §8.1](rfc-6-block-metadata.md#8.1%20Covering%20lookup));
3. resolves each part by [RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function):

| Metadata | Residency | The engine |
| --- | --- | --- |
| hole, or a zero ref | **Absent** | returns zeros |
| uncarved | **Lost** | fails the read, and reports data loss naming the file and extent |
| carved | **Remote** | gets the chunk, verified ([RFC 3 §4.1](rfc-3-syncer.md#4.1%20One%20fetch%2C%20two%20consumers)) |
| past end of file | — | returns a short read |

"Uncarved" and "past end of file" are judged against existence with the
journal's uncommitted operations applied ([§4.1](#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not)). The engine **MUST** compute
this per request, **MUST NOT** keep its result, and **MUST NOT** return zeros for
any part but a hole or a zero ref. A fetch is issued per missing extent, never per
window.

### 6.2 The reply is served from the fetched bytes

The engine answers the read from the verified bytes the fetch returned. It **MUST
NOT** answer by re-reading the journal after a fill: that makes the reply wait on
the fill, and makes a fill failure fail a read whose bytes were correct in hand.

When it fills, the engine passes `Fill` exactly the bytes it fetched, with the
`asOf` version `ReadAt` returned before resolving. The journal refuses the fill if
the file changed after it, so a write that landed meanwhile wins ([RFC 1 §3.4](rfc-1-journal.md#3.4%20Fill)).
A refused fill is not an error.

### 6.3 Filling is a decision

[RFC 0 §6.2](rfc-0-data-lifecycle.md#6.2%20Fill) gives the fill decision to the engine. **Proposal — fill a demanded
extent unless** the journal's free capacity is below a low-water mark reserved for
writes, the read is part of a sequential scan longer than the readahead window, or
the fetch served a pre-warm asked to yield ([§6.4](#6.4%20Speculation%20is%20planned%20here%2C%20and%20yields%20to%20demand%20and%20to%20writes)). A declined fill still
answers the read. Overturned by a hit-rate and write-refusal measurement comparing
fill-always, this rule and fill-never on a large-file workload.

### 6.4 Speculation is planned here, and yields to demand and to writes

The engine decides what to fetch before it is asked, and issues those fetches as
speculative, which the fetcher never lets delay a demand ([RFC 3 §4.4](rfc-3-syncer.md#4.4%20Speculation%20does%20not%20delay%20demand)). The engine
labels each transfer with its class — demand for a read waiting on it, background
for offload and GC relocation, speculation for read-ahead and pre-warm — and the
syncer schedules the classes in that order; the engine sets no other priority.

**Read-ahead** follows an observed sequential pattern, a window of blocks ahead of
the reader, bounded in bytes; a random access resets it.

**Pre-warm** is an explicit request over a share or subtree, run on a flow of its
own so it never holds the share's demand reads. It **MUST NOT** drive the journal
towards refusing writes. **Proposal:** pre-warm fills only while free capacity is
above the write low-water mark, pauses below it, and cancels its queued fetches
on a write that meets capacity pressure; it reports how far it got and is
re-issuable. Overturned by a measurement showing a fixed reservation churns less.

### 6.5 An unreachable remote fails the read, distinguishably

A **Remote** extent whose fetch cannot complete — the remote is unreachable, or
the demand deadline expires — **MUST** fail the read with an error distinguishable
from **Lost** ([RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model)). The first is transient; the second is data loss.

### 6.6 Allocation answers from the hole set

`SEEK_DATA`, `SEEK_HOLE` and sparse-read replies are answered from block
metadata's hole set and zero refs, with the journal's uncommitted operations
applied ([RFC 7 §9.3](rfc-7-namespace-metadata.md#9.3%20Residency%20is%20not%20an%20attribute)). They **MUST NOT** be answered from what the journal holds,
which cannot tell a hole from an evicted extent.

### 6.7 An absent object is re-resolved while its location moves

Relocation moves a chunk to a new block and leaves the old block to sweep
([RFC 9 §4.3](rfc-9-gc.md#4.3%20A%20reader%20can%20hold%20the%20old%20location)). A read that resolved the chunk before the move can find the old
object gone. Refs name hashes, not blocks ([RFC 6 §2.5](rfc-6-block-metadata.md#2.5%20Refs%20name%20hashes%2C%20never%20blocks)), so the chunk is still
reachable.

When the remote tier reports the object a chunk's get named as absent, the engine
**MUST** resolve the chunk's location again from block metadata and get it there,
and **MUST** keep doing so **while each resolution names a different block**,
bounded by the read's deadline. It **MUST** fail the read as **Lost** when a
resolution names a location that already missed, or finds no chunk record. A
chunk can move more than once while a read is in flight; a location that misses
twice is not moving.

A ranged read that fails verification is corrupt, not stale: a name is put only
by its one attempt, whose retries write the same bytes, so a recorded position is
always right ([RFC 6 §2.2](rfc-6-block-metadata.md#2.2%20Chunk)). It fails
the read as corruption and **MUST NOT** trigger re-resolution; neither does a
transport error or a timeout.

### 6.8 A cold read asks for chunks, or for the block

A fetch names the chunks it needs, or asks for the whole block ([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)).
Every request pays a round trip; every byte fetched and not read is bandwidth
spent for nothing.

**Proposal:** a read asks for **only the chunks it covers** when its missing bytes
are at most a quarter of the block **and** it is not sequential — it neither
starts at the block's beginning nor continues where the file's previous read
ended. Otherwise it asks for **the whole block**. Read-ahead and pre-warm always
ask for whole blocks. The quarter is borrowed from prior art; overturned by
comparing bytes fetched and read latency across a few thresholds.

## 7. Local space

Capacity is the device journal's, shared by the shares on it with per-share
accounting and fair limits ([RFC 1 §7](rfc-1-journal.md#7.%20Capacity)). The engine of each share decides for
its own files; pressure on the journal is pressure on every engine using it. An
upload holds one chunk per worker in memory and needs no local space.

### 7.1 Eviction is chosen here, and needs no new record

The engine selects what to evict and calls `Release` on it. **Proposal:** coldest
first by last access, in units the journal can free ([RFC 1 §8.1](rfc-1-journal.md#8.1%20Releasing%20storage)), until a
target set by capacity pressure is met.

The record that makes eviction safe is the offload commit, made before the
offloaded bit was set ([RFC 6 §4.3](rfc-6-block-metadata.md#4.3%20The%20commit%20is%20the%20report%27s%20return%20edge)); a carved extent the journal does not hold
resolves to **Remote**. Eviction therefore writes nothing to metadata.

### 7.2 A capacity refusal comes back here

The journal refuses a write it cannot reserve for, and does not evict for itself
([RFC 1 §7](rfc-1-journal.md#7.%20Capacity)). The engine answers:

1. evict ([§7.1](#7.1%20Eviction%20is%20chosen%20here%2C%20and%20needs%20no%20new%20record)), then retry;
2. if what is held is dirty and the remote is available, offload it at once,
   evict what that made durable, then retry;
3. if still nothing frees space, repack ([§7.3](#7.3%20Repack%20is%20triggered%20here)), then retry;
4. otherwise refuse the write with a distinguishable error.

The retry is bounded by the caller's deadline ([RFC 0 §10.3](rfc-0-data-lifecycle.md#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)). The refusal names
its cause as space, so a protocol answers it as "no space" and not as an I/O error.

### 7.2.1 Writes are paced before the limit, not stopped at it

Between a **soft threshold** of dirty bytes and the limit, the engine delays each
write in proportion to how far past the threshold the journal is, scaled to the
measured drain rate, so writes slow to the drain rate instead of running into a
wall. Above the soft threshold every file with dirty bytes is offload-eligible.
The delay is bounded by the caller's deadline, and is computed from
the drain rate, not refreshed by it: a trickle of progress does not extend it. **Proposal:** a soft threshold at
half the journal's capacity and a delay rising linearly to the drain rate at the
limit. Overturned by a curve that keeps p99 submission latency lower at the same
throughput.

### 7.3 Repack is triggered here

The engine requests a repack when the journal's statistics show recoverable
storage ([RFC 1 §8.3](rfc-1-journal.md#8.3%20Accounting), [§8.4](rfc-1-journal.md#8.4%20Open%20descriptors), [§5.2](rfc-1-journal.md#5.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)). It **MUST** request one when the
journal is at capacity and nothing is evictable ([RFC 1 §8.2](rfc-1-journal.md#8.2%20Repack)).

### 7.4 Nothing but durability makes an extent unevictable

Locks, deny modes, delegations, open handles and snapshots **MUST NOT** make an
extent ineligible for eviction ([RFC 7 §8.6](rfc-7-namespace-metadata.md#8.6%20Locks%20do%20not%20pin%20bytes), [RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). An operator's retention
pin **MAY** exclude a share from eviction, and the engine **MAY** suspend eviction
while the remote is unreachable. Both are availability policy, never what keeps
content safe ([§3.2](#3.2%20Policy%20never%20makes%20an%20action%20safe)).

### 7.5 GC is not scheduled here

GC is one service per remote namespace, holding its own lease, and it schedules
itself: cadence, triggers and the relocation threshold are its own ([RFC 9](rfc-9-gc.md)).
Relocation is on by default. The engine neither composes nor schedules it; GC
reaches block metadata and the remote store through its own views, and opens its
own syncer flow for relocation.

## 8. Health and failure

### 8.1 Health is derived from recent outcomes, offload included

Whether a store is usable is the syncer's to say ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)); the engine reads
it through its flow's `Healthy(d)` and **MUST NOT** probe the store itself. The
engine opens one flow per share and **SHOULD** skip an offload pass for a share
whose store is put-unhealthy.

Health is per direction. The engine gates offload on `Healthy(Put)` and nothing
else; reads go to the syncer, which fails them at once only while the store is
get-unhealthy. A put-unhealthy store never refuses a read, and the share reports
the two directions as separate conditions.

**A drifted service setting stops puts.** When the store's `Recheck` reports drift
([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)), puts and deletes to the store stop until a later `Recheck` passes. The
engine treats that as put-unhealthy — it skips offload passes — and reports a
share condition naming the drifted setting; reads carry on.

> [!important] Pending review — share health per direction, drift stops puts
> Health was one flag per store. Put and get are now judged apart, so a failing
> put path never refuses reads, and a drift found by `Recheck` stops offload
> without touching the read path.

Share health adds what only the engine sees: sustained inability to offload
**MUST** be a health condition of the share ([RFC 0 §10.2](rfc-0-data-lifecycle.md#10.2%20No%20state%20requires%20intervention%20to%20leave)), distinguishable from
the remote being unreachable — an offload that fails on metadata with the remote
healthy is a wedge a probe cannot see. The engine **MAY** slow offload attempts
under ill health, and **MUST NOT** stop them without something independent that
will observe recovery.

### 8.2 Every condition in RFC 0 §10 has its engine behaviour here

Only what the engine adds to [RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model):

| Condition | The engine |
| --- | --- |
| Remote unavailable | keeps accepting writes while capacity allows; backs off offloads; fails **Remote** reads distinguishably ([§6.5](#6.5%20An%20unreachable%20remote%20fails%20the%20read%2C%20distinguishably)) |
| Journal at capacity | runs [§7.2](#7.2%20A%20capacity%20refusal%20comes%20back%20here) |
| Metadata unwritable | fails stability replies; offload fails and reports the offload condition ([§8.1](#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included)) |
| Crash | recovers, re-applies existence, then re-offers ([§2.5](#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)) |

### 8.3 The engine surfaces no serialization conflict

A metadata conflict **MUST** be retried under the caller's deadline and **MUST
NOT** reach a client as an I/O error (RFC 0 I8). An offload commit's deadline is
its pass's own.

### 8.4 How far behind durability is, is observable

The engine **MUST** report, per share and summed: dirty bytes, the drain rate
over a recent window, and the time to drain at that rate; and **MUST** offer a way
to wait until every byte written before the call is durable remotely. A benchmark
of the write path **MUST** stop its clock at that wait, not at the last
acknowledgement ([§12.4](#12.4%20Benchmarks)).

The engine **MUST** also report the age of the oldest unoffloaded extent each
share's journal holds, from the journal's `OldestDirty` ([RFC 1 §3.7](rfc-1-journal.md#3.7%20State%20introspection)), and raise an alert when it passes a configured
bound. Drain time says how long the backlog would take; the oldest age says
whether one extent is stuck behind it — a file whose offload keeps failing while
the rest drains stays invisible to every sum.

> [!important] Pending review — oldest unoffloaded age, transfer classes
> A writeback cache whose sums look healthy can hold one extent unoffloaded
> indefinitely; the age of the oldest one is the signal. Transfer classes follow
> the syncer's scheduling.

## 9. The facade

### 9.1 One facade, shaped like content

Adapters reach content through one surface, never through a component:

| Operation | Composes |
| --- | --- |
| `Write(file, off, bytes)` | [§4.1](#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not) |
| `Read(file, off, len)` | [§6](#6.%20The%20read) |
| `Commit(file)` | the stability point ([§9.4](#9.4%20Commit%20is%20answered%20by%20the%20journal)) |
| `Truncate(file, size)` | journal `Truncate`, then [RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation) in one transaction, under the guard ([§4.3](#4.3%20The%20offload%20guard%20is%20narrow)) |
| `Deallocate(file, off, len)` | [§9.2](#9.2%20Deallocate%20records%20a%20hole%3B%20it%20does%20not%20write%20zeros), under the guard |
| `Release(file)` | journal `Delete`, then [RFC 6 §6.4](rfc-6-block-metadata.md#6.4%20Delete), under the guard — the implementation of [RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees) |
| `Clone(src, dst, …)` | [§9.3](#9.3%20Clone%20adopts%20refs%2C%20and%20offloads%20uncarved%20content%20first), under both files' guards until done |
| `Size`, `Times`, `Allocation` | committed existence with the journal's uncommitted operations applied ([§4.1](#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not)) |
| `Stats`, `Health`, `WaitDurable` | [§8](#8.%20Health%20and%20failure) |

The facade **MUST NOT** return a component to its caller: a caller holding one can
do what the facade orders, out of order.

The facade **MUST** be callable across a network, because under ownership a
request can arrive at a node that does not own the file ([RFC 11](rfc-11-ownership.md)): bodies are
streamed, no operation takes a callback, and every operation is safe to retry:
a write, truncate or deallocate repeated with the same arguments leaves the same
content, at a new version. Only a replica's `Apply` recognises a retry under the
same version as a repetition it ignores ([RFC 10 §2.5](rfc-10-journal-replication.md#2.5%20The%20journal%20extension)); the facade's
operations assign new versions.

**Each removal settles its marker.** After a truncate's, deallocate's or
release's first metadata transaction commits, the engine calls `Settle(id, v)`
at the removal's version ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Settle%20and%20Since)), so the journal drops the marker. A
crash before that leaves the marker for recovery ([§2.5](#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)).

Signatures are indicative; the obligations are normative.

```go
type Engine interface {
    Write(ctx context.Context, id Identity, file FileID, off int64, r io.Reader, n int64) error
    Read(ctx context.Context, id Identity, file FileID, off, n int64, w io.Writer) (int64, error) // ErrLost, ErrUnavailable, ErrCorrupt
    Commit(ctx context.Context, file FileID) error // the stability point (§9.4)
    Truncate(ctx context.Context, file FileID, size int64) error
    Deallocate(ctx context.Context, file FileID, off, n int64) error
    Release(ctx context.Context, file FileID) error
    Clone(ctx context.Context, src, dst FileID, srcOff, dstOff, n int64) error
    Size(ctx context.Context, file FileID) (int64, error)
    Times(ctx context.Context, file FileID) (mtime, ctime time.Time, err error)
    Allocation(ctx context.Context, file FileID, off int64) (Span, error)
    WaitDurable(ctx context.Context) error // every byte written before the call is durable remotely (§8.4)
    Stats() Stats
    Health() Health
    Close() error
}

var (
    ErrLost        = errors.New("engine: content lost")       // §6.1, reported as data loss
    ErrUnavailable = errors.New("engine: remote unavailable") // §6.5, transient
    ErrCorrupt     = errors.New("engine: content corrupt")    // §6.7
    ErrNoSpace     = errors.New("engine: journal full")       // §7.2
)
```

### 9.2 Deallocate records a hole; it does not write zeros

The journal stops holding the range at a new version, then one transaction adds
the hole, drops or narrows the refs over it and records the removal
([RFC 6 §3.5](rfc-6-block-metadata.md#3.5%20Operations%20that%20make%20holes), [§6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)). It **MUST NOT** stage zeros through the write path:
zeros staged as data consume journal capacity in proportion to the range, so a
large deallocation could refuse writes.

### 9.3 Clone adopts refs, and offloads uncarved content first

On a share with a remote tier, a clone is a batched removal
([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)):

1. offload every source extent the journal holds newer than its covering ref's
   `newest` — not only uncarved extents: a durable extent the metadata has not
   caught up with would otherwise be cloned stale;
2. under the source's and the destination's guards, taken in file-identity
   order, remove the destination range in the journal at version *v*, and record
   the removal as a deallocate of that range (phase 1);
3. in batches of at most K refs in source-offset order, drop the destination's
   refs below *v* in the batch's range, then write the source's refs re-versioned
   at *v* and count their chunks; a batch fails if any chunk has been retired
   ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)). Source holes stay holes (phase 2).

When source and destination are one file and the ranges overlap, each batch reads
its source refs before applying the destination drop that could remove them. A
clone that fails for good is undone by a removal of the destination range,
batched the same way. The source's guard is held until the clone is done, and the
destination range is not served until then. An unfinished clone resumes at startup before the
destination is served ([§2.5](#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)). **Proposal:** offload first rather
than copy bytes, so clone has one path and the destination shares content from
its first byte. On a share with no remote tier nothing is carved ([§4.6](#4.6%20A%20share%20with%20no%20remote%20tier%20never%20reports%20durability)), so a
clone copies bytes through the destination's write path.

> [!important] Pending review — clone as a batched removal
> A clone of many refs no longer fits one transaction. It offloads every source
> extent newer than its ref, deallocates the destination, then copies refs in
> batches; the heading's "uncarved" now reads as "not yet in metadata". Steps now
> follow RFC 6 §6.6: per-batch drop then adopt, overlap order, undo on failure.

### 9.4 Commit is answered by the journal

A client's flush — NFS `COMMIT`, SMB `FLUSH`, `fsync`, a stable write — is a
**stability point** and reaches the facade as `Commit`. The facade first calls the
journal's `Sync` on the file — at every member, where replication is composed
([RFC 10](rfc-10-journal-replication.md)) — and only then group-commits its pending existence under the owner
epoch ([§4.1](#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not)), and answers once both are done. Every existence commit — a
stability point, an offer's capture, a removal's first transaction, recovery —
**MUST** `Sync` the file first: the journal publishes an extent once its record is
written, not once it is durable ([RFC 1 §10.4](rfc-1-journal.md#10.4%20What%20must%20be%20atomic)), so existence committed over
unsynced records could outlive them.

> [!important] Pending review — existence commits sync first
> The journal now publishes on write, not on sync, so the engine syncs the file
> before any existence commit rather than relying on a published extent being
> durable. It **MUST NOT** wait for an offload, and does not
schedule one, carve, or touch the remote tier.

This is the only acknowledgement policy: the journal is required to be durable
([RFC 1 §6.2](rfc-1-journal.md#6.2%20Sync%20policy)), so what it has synced survives a crash, and offload carries it to
the remote on its own schedule. A remote-acknowledged commit would bound every
`fsync` by a put and turn a remote outage into client write errors.

### 9.5 The facade writes no residency

The facade **MUST NOT** offer an operation that tells the journal an extent is
remote, cold, or pinned. Residency is computed ([RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function)).

## 10. Invariants

| # | Invariant |
| --- | --- |
| E1 | The engine persists nothing, and no answer it gives outlives the request that asked. |
| E2 | Every capability is a declared parameter, supplied at construction; none is negotiated at run time. |
| E3 | No policy gate is the only thing preventing data loss. |
| E4 | An offload reports exactly the extents whose commits succeeded, block by block, in any order. |
| E5 | A share with no remote tier reports nothing durable. |
| E6 | The offload guard is held only to capture an offer and to commit; removals hold it across their journal step and first metadata transaction, and a clone holds its source's until done; every commit carries the owner epoch. |
| E7 | The dedup oracle never answers from a block that has not committed, and a chunk repeated across blocks in flight is carried in each. |
| E8 | A block's name is minted once per put attempt from scope, a fresh nonce, chain ID and ordered chunk hashes; no other put ever uses it, and a durable intent carrying the owner epoch precedes the put. |
| E9 | A read returns zeros only for a hole or a zero ref, and fails distinguishably for **Lost**, corruption and an unreachable remote. |
| E10 | A read's reply never depends on the fill. |
| E11 | Only durability decides whether an extent may be evicted. |
| E12 | Sustained offload failure is a health condition, distinct from an unreachable remote. |
| E13 | No background work outlives a component it uses. |
| E14 | A get that finds its object absent is re-resolved while the location changes, and fails as **Lost** only when a location misses twice or no record exists. |
| E15 | Existence is answered from the journal until the stability point, committed there, and re-applied from the journal at recovery before a file is served; an offer covers only committed existence. |
| E16 | An upload survives a removal under it, and its commit drops exactly the refs the removal covers. |
| E17 | Correctness never depends on the per-file guard; the metadata store's conflicts order commits. |
| E18 | Widening reads only durable neighbours the journal offered frozen. |
| E19 | Every existence commit syncs the file in the journal first. |
| E20 | An artificial stretch end leaves its tail Dirty and re-offered from `consumed`, except past the age ceiling; a short last block waits no longer than that ceiling. |
| E21 | A put-unhealthy or drifted store stops offload and never refuses a read. |

> [!important] Pending review — E8, E17 to E21
> E8 now states minted names and put intents; E17 and E18 are new with §4.3 and
> §5.6; E19 to E21 with §9.4, §5.6 and §8.1.

## 11. Observability

The engine exports what only it can see; each component exports its own. Every
metric is labelled by share.

| Answers | Metric | Type |
| --- | --- | --- |
| dirty bytes, drain rate, time to drain ([§8.4](#8.4%20How%20far%20behind%20durability%20is%2C%20is%20observable)) | `dittofs_engine_dirty_bytes`, `dittofs_engine_drain_bytes_per_second`, `dittofs_engine_drain_seconds` | gauge |
| offload passes, labelled `result` ([§8.1](#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included)) | `dittofs_engine_offload_passes_total` | counter |
| passes aborted as wholly removed; commits that found their intent gone | `dittofs_engine_passes_aborted_total`, `dittofs_engine_intent_missing_total` | counter |
| age of the oldest unoffloaded extent the journal holds, from `OldestDirty` ([RFC 1 §3.7](rfc-1-journal.md#3.7%20State%20introspection)); alert when it passes the share's upload-age bound | `dittofs_engine_oldest_unoffloaded_seconds` | gauge |
| busy time of each stage — carve, assemble, upload, commit ([§12.4](#12.4%20Benchmarks)) | `dittofs_engine_stage_busy_ratio` | gauge |
| stability points and the operations each committed | `dittofs_engine_stability_points_total`, `dittofs_engine_pending_existence_ops` | counter, histogram |
| reads, labelled `class` = `journal`, `hole`, `remote`, `lost`, `corrupt` or `unavailable` | `dittofs_engine_reads_total` | counter |
| fetches, labelled `shape` = `chunks` or `block` ([§6.8](#6.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block)), and bytes fetched against bytes read | `dittofs_engine_fetches_total`, `dittofs_engine_fetch_bytes_total` | counter |
| fills, labelled `result` = `done`, `declined` or `refused` ([§6.3](#6.3%20Filling%20is%20a%20decision)) | `dittofs_engine_fills_total` | counter |
| re-resolutions after an absent object ([§6.7](#6.7%20An%20absent%20object%20is%20re-resolved%20while%20its%20location%20moves)) | `dittofs_engine_reresolves_total` | counter |
| evictions and bytes freed; pacing delays; refusals ([§7](#7.%20Local%20space)) | `dittofs_engine_evicted_bytes_total`, `dittofs_engine_pacing_seconds`, `dittofs_engine_write_refusals_total` | counter, histogram, counter |
| offloaded bits cleared by the consistency check ([§2.5](#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)) | `dittofs_engine_ledger_mismatches_total` | counter |

A **Lost** or corrupt read logs the file and extent at `Error`, once per extent.
A share entering or leaving an offload health condition logs at `Warn`. A refused
write logs at `Warn`, rate-limited. A ledger mismatch logs the file and extent at
`Warn`.

## 12. Conformance

Every check runs against the engine as production composes it, in the tiers and
under the rules of the [index](rfc-index.md).

### 12.1 Group A — lost or wrong content

| Requirement | Check |
| --- | --- |
| [§3.2](#3.2%20Policy%20never%20makes%20an%20action%20safe) no gate is safety | Disable suspension, pins and health gating; evict as eagerly as possible; run every other Group A check. Assert all pass. |
| [§4.6](#4.6%20A%20share%20with%20no%20remote%20tier%20never%20reports%20durability) local-only | On a share with no remote, offload repeatedly and force eviction. Assert no offloaded bit is set and every read returns its bytes. |
| [§4.5](#4.5%20The%20callback%20returns%20only%20what%20committed) per-block reporting | Fail the second of three block commits. Assert the journal marks exactly the first and third blocks' extents. |
| [§4.1](#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not) journal authority | Write without a stability point; assert `Size`, `Allocation` and reads reflect the write. Crash; assert recovery re-applies it before the file is served. |
| [§4.3](#4.3%20The%20offload%20guard%20is%20narrow) guard across a removal | Stall a truncate between its journal step and its transaction; trigger an offload. Assert the offer waits, and no ref lies past `size` after both finish. |
| [§4.4](#4.4%20Transfers%20survive%20removals%20under%20them) transfer survives a removal | Stall a pass's upload; truncate the file below the offered range; release the upload. Assert the upload completes, the commit drops the refs past the new size and applies the rest, the journal served `offered` throughout, and the truncated range reads as past end of file. Repeat with deallocate, release and a clone onto the file. |
| [§2.5](#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing) restart re-offer | Crash after a put and before its commit; restart. Assert the extents are offered again under a new name, the first object stays unrecorded with its intent, collection removes both once the epoch is superseded, and reads are correct. Crash after a commit, before `report`; assert the re-offer adopts every chunk and the extents become evictable. |
| [§2.5](#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing) recovery through `Since` | Crash after a truncate's journal step, before its removal record. Restart; assert `Since` yields the marker, existence applies it before the file is served, and `Settle` drops it. Crash a removal between batches; assert it resumes and the range reads as removed throughout. |
| [§5.4](#5.4%20A%20block%27s%20name%20is%20minted%20here%2C%20and%20its%20intent%20recorded%20before%20the%20put) minted names and intents | Retry a put with an unknown outcome; assert the same name and bytes. Re-offer the same content in a new pass; assert a new name. Remove the intent before the commit; assert the commit fails and the content is re-offered. Delete an object once record and intent are gone, then land a delayed delete of it; assert no committed block is touched. |
| [§5.6](#5.6%20A%20run%20is%20what%20the%20journal%20offers%2C%20widened%20only%20to%20re-tile) widening | Widen a run over a durable neighbour and release the neighbour mid-pass; assert the release waits for the pass and the new refs read correctly. |
| [§4.3](#4.3%20The%20offload%20guard%20is%20narrow) guard is not a fence | Run two engines, each believing it owns one file, with separate guards. Assert the store refuses the stale owner's commits on both paths. |
| [§5.1](#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output) zero chunks | Offload an all-zero range. Assert no put carries it, reads return zeros with no fetch, and `SEEK_HOLE` reports it. |
| [§5.3](#5.3%20The%20dedup%20oracle%20never%20sees%20an%20uncommitted%20block) in-flight dedup | Carve a chunk into two blocks in flight; fail the first put after the second commits. Assert the second block carries the chunk and a read succeeds. |
| [§5.3](#5.3%20The%20dedup%20oracle%20never%20sees%20an%20uncommitted%20block) retired adoption | Retire a chunk between the oracle's answer and the commit. Assert the commit fails, the run is re-offered carrying the chunk, and the read succeeds. |
| [§6.1](#6.1%20Resolution%20is%20a%20join%2C%20computed%20per%20request) the join | Drive every row of [§6.1](#6.1%20Resolution%20is%20a%20join%2C%20computed%20per%20request), including an uncarved extent the journal lost. Assert **Lost** fails — a check of the other rows passes a build that serves zeros. |
| [§6.2](#6.2%20The%20reply%20is%20served%20from%20the%20fetched%20bytes) fill cannot fail a read | Make `Fill` fail. Assert the read returns the fetched bytes. |
| [§6.2](#6.2%20The%20reply%20is%20served%20from%20the%20fetched%20bytes) fill loses to a write | Stall a fetch; write the extent; release the stall. Assert the written bytes survive. |
| [§6.7](#6.7%20An%20absent%20object%20is%20re-resolved%20while%20its%20location%20moves) re-resolution | Relocate a chunk twice between a reader's resolution and its gets, sweeping each old block. Assert the read succeeds. Retire the chunk outright; assert **Lost**, not zeros, and not after a deadline. Make a ranged read fail verification; assert a corruption error and no re-resolution. |
| [§9.2](#9.2%20Deallocate%20records%20a%20hole%3B%20it%20does%20not%20write%20zeros) deallocate | Deallocate a range larger than free journal capacity. Assert it succeeds, reads as zeros, and consumes no journal capacity. |
| [§9.4](#9.4%20Commit%20is%20answered%20by%20the%20journal) sync before existence | Write, then `Commit`, a truncate and an offer capture, each with the storage seam dropping unsynced records at a host crash injected right after the existence commit. Assert every committed `size` is backed by synced records: no read of committed existence fails as **Lost**. Remove the `Sync`: the check fails. |
| [§5.6](#5.6%20A%20run%20is%20what%20the%20journal%20offers%2C%20widened%20only%20to%20re-tile) stretch ends | Stream a file across passes cut by `limit`. Assert no pass reports bytes past `consumed`, the next pass starts at a content boundary, and the chunking equals one pass over the whole file. Stop writing: assert the tail is cut and durable within the age ceiling. |

### 12.2 Group B — wedging

| Requirement | Check |
| --- | --- |
| [§8.1](#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included) health per direction | Fail every put while gets succeed. Assert offload passes stop, reads of **Remote** extents succeed, and the share reports a put condition only. Make `Recheck` report drift: assert puts stop and reads continue until a later `Recheck` passes. |
| [§8.1](#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included) offload health | Make every offload commit conflict with the remote healthy. Assert an offload condition, distinct from remote-unreachable, before the journal fills. |
| [§7.2](#7.2%20A%20capacity%20refusal%20comes%20back%20here) refusal loop | Fill to capacity with durable content. Assert a write succeeds after the engine evicts. Repeat with dirty content and the remote available; assert the engine offloads, evicts and accepts. |
| [§2.3](#2.3%20A%20share%20is%20one%20engine) shared journal | Two shares on one device journal; fill one. Assert the other's writes are not refused. |
| [§6.4](#6.4%20Speculation%20is%20planned%20here%2C%20and%20yields%20to%20demand%20and%20to%20writes) pre-warm yields | Pre-warm more than free capacity while writing. Assert no write is refused. |
| [§2.5](#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing) join | Close with an offload parked in a stalled put. Assert no component is closed while the pass runs. |

### 12.3 Group C — composition

| Requirement | Check |
| --- | --- |
| [§2.2](#2.2%20Capabilities%20are%20parameters%2C%20never%20assertions) no assertions | Remove one method from each capability's provider. Assert the build fails, not the behaviour. |
| [§2.4](#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced) settings | Configure `Min ≥ Target`. Assert construction fails. Change a share's profile. Assert it is reported as a migration. |
| [§2.5](#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing) consistency check | Restart with a ledger bit no ref justifies. Assert eviction was not delayed, and the bit is reported and cleared before that extent is released. |

A sink that always succeeds **MUST NOT** stand in for Group A, and a single-file
rig **MUST NOT** stand in for [§4.4](#4.4%20Transfers%20survive%20removals%20under%20them) or [§5.3](#5.3%20The%20dedup%20oracle%20never%20sees%20an%20uncommitted%20block): both need two passes or blocks in
flight at once.

### 12.4 Benchmarks

These measure the pipeline, against a local emulator of the remote service per
change and each real service daily.

| # | Measures | Setup | Target |
| --- | --- | --- | --- |
| E1 | offload to durable | sustained writes of non-deduplicating data larger than the journal | durable MiB/s ≥ 80% of the sizing tool's raw figure ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)) for the same store and pool sizes |
| E2 | small files | create, write and close 10^4 and 10^5 files of 64 KiB | files/s within 20% across directory sizes; puts per file ≤ 64 KiB / block target, rounded up |
| E3 | pacing | E1 below and above the soft threshold ([§7.2.1](#7.2.1%20Writes%20are%20paced%20before%20the%20limit%2C%20not%20stopped%20at%20it)) | p99 submission latency ≤ 50 ms, max ≤ 500 ms, durable throughput within 10% of E1 |
| E4 | cold reads | random 4 KiB and sequential reads of evicted content | random: one request per read, p99 ≤ 2 store round trips; sequential: bytes fetched per byte read ≤ 1.1 |
| E5 | group commit | 64 writers, `fsync` every 1 MiB | metadata transactions per stability point ≤ 1 per journal |
| E6 | one large file | one file sequentially written to 20× the journal's capacity | durable MiB/s over the last tenth within 10% of the first tenth; extents made durable while the file is still being written, block by block ([§4.5](#4.5%20The%20callback%20returns%20only%20what%20committed)), never only at the end of a pass |

Method: measure the raw link, raw object storage and the full stack on the same
hosts at the same time, and report each as a fraction of the one below; use data
that does not deduplicate; stop the clock at durability ([§8.4](#8.4%20How%20far%20behind%20durability%20is%2C%20is%20observable)); report every
stage's occupancy beside E1; record the network distance to the store; run long
enough to exhaust the local device's write cache.

## 13. Open questions

1. **Fill policy parameters** ([§6.3](#6.3%20Filling%20is%20a%20decision)): the low-water mark and scan threshold
   are unmeasured.
2. **Pre-warm's yield mechanism** ([§6.4](#6.4%20Speculation%20is%20planned%20here%2C%20and%20yields%20to%20demand%20and%20to%20writes)): pause-and-cancel against a fixed
   reservation has not been run.
3. **Randomised assembly** ([§5.5](#5.5%20Assembly%20is%20sequential%2C%20and%20may%20change%20without%20migration)): what an observer recovers from object
   sizes at this system's block sizes is unmeasured.
4. **Key scope** ([§5.4](#5.4%20A%20block%27s%20name%20is%20minted%20here%2C%20and%20its%20intent%20recorded%20before%20the%20put)): whether any deployment wants cross-share dedup enough
   to accept one metadata store per remote namespace.
5. **Offload thresholds** ([§4.2](#4.2%20Offload%20is%20scheduled%20here)): the proposed bounds are historical defaults.
6. **Unspecified policy**: upload delay for data young enough to be overwritten,
   and manual sync.

## Appendix A — where the current code differs

One line per requirement.

| Requirement | Code today |
| --- | --- |
| [§2.1](#2.1%20The%20engine%20is%20the%20composition%20root%2C%20and%20the%20only%20one) one composition root, no setters | split between the runtime and the engine; setters wire the remote store and metrics on a serving engine |
| [§2.2](#2.2%20Capabilities%20are%20parameters%2C%20never%20assertions) no type assertions | about fifteen capabilities negotiated by assertion, each with a silent fallback |
| [§2.3](#2.3%20A%20share%20is%20one%20engine) one journal per device | one journal per share |
| [§2.4](#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced) settings refused | invalid chunking settings replaced by defaults; no profile record |
| [§2.5](#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing) start order | no floor, no existence replay, no ledger; a bounded join, then close under running loops |
| [§3.1](#3.1%20Policy%20is%20decided%20here%20and%20executed%20below), [§4.2](#4.2%20Offload%20is%20scheduled%20here) offload policy here | thresholds are journal configuration |
| [§4.1](#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not) the facade orders a write; existence at the stability point | adapters call authorise and existence around a stage-only write |
| [§4.2](#4.2%20Offload%20is%20scheduled%20here), [§8.1](#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included) offload never stopped | passes skipped while the remote is unhealthy, cleared only by the probe |
| [§4.3](#4.3%20The%20offload%20guard%20is%20narrow) narrow per-file guard | a journal shard lock and a striped engine lock; removals do not take it |
| [§4.4](#4.4%20Transfers%20survive%20removals%20under%20them) transfers survive removals | no removal is checked at commit |
| [§4.6](#4.6%20A%20share%20with%20no%20remote%20tier%20never%20reports%20durability) no durability without a remote | a local sink reports extents durable |
| [§5.1](#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output) assembly in the engine, streamed | assembly in the carver; chunks copied through three buffers |
| [§5.2](#5.2%20The%20target%20counts%20carried%20bytes) target counts carried bytes | adopted chunks count |
| [§5.3](#5.3%20The%20dedup%20oracle%20never%20sees%20an%20uncommitted%20block) no binding guard | an in-process adoption guard |
| [§5.4](#5.4%20A%20block%27s%20name%20is%20minted%20here%2C%20and%20its%20intent%20recorded%20before%20the%20put) minted name, intent before put | random names, no intent; the name is not recomputable from the header |
| [§6.1](#6.1%20Resolution%20is%20a%20join%2C%20computed%20per%20request) per missing extent, hole vs uncarved | the whole window fetched; an uncovered extent reads as zeros |
| [§6.2](#6.2%20The%20reply%20is%20served%20from%20the%20fetched%20bytes), [§6.3](#6.3%20Filling%20is%20a%20decision) reply independent of fill | every fetch fills and the read re-reads the journal |
| [§6.4](#6.4%20Speculation%20is%20planned%20here%2C%20and%20yields%20to%20demand%20and%20to%20writes) pre-warm yields | runs until the journal refuses |
| [§6.6](#6.6%20Allocation%20answers%20from%20the%20hole%20set) allocation from the hole set | answered from the journal joined with the refs |
| [§6.7](#6.7%20An%20absent%20object%20is%20re-resolved%20while%20its%20location%20moves) re-resolution | once only; a second resolution naming no location reads as "not uploaded yet" |
| [§7.1](#7.1%20Eviction%20is%20chosen%20here%2C%20and%20needs%20no%20new%20record), [§7.2](#7.2%20A%20capacity%20refusal%20comes%20back%20here) eviction and refusal here | the journal evicts and refuses for itself |
| [§7.5](#7.5%20GC%20is%20not%20scheduled%20here) GC not scheduled here | a process-wide ticker in the engine layer |
| [§8.1](#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included) offload in health | a failed pass reaches no health state |
| [§9.1](#9.1%20One%20facade%2C%20shaped%20like%20content) no component returned | the facade returns its journal and remote store |
| [§9.2](#9.2%20Deallocate%20records%20a%20hole%3B%20it%20does%20not%20write%20zeros) deallocate records a hole | writes zeros through the journal |
| [§9.4](#9.4%20Commit%20is%20answered%20by%20the%20journal) commit answered by the journal | a per-share setting makes commit wait for an inline offload |
| [§9.5](#9.5%20The%20facade%20writes%20no%20residency) no residency writes | the facade marks ranges remote and pins journal versions |
| E1 no dead state | a read cache is started and never consulted |
