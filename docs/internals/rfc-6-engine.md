# RFC 6 — the engine

**Status:** draft.
**Depends on:** RFC 0, for the terms, the residency function, the invariants and
the failure model. RFC 1, 2, 3, 4, 5 and 8 specify the components this one
composes; each has already deferred a decision here, and Appendix A lists every
one of them. Nothing here redefines any of them.
**Audience:** anyone changing `pkg/block/engine`, the per-share composition in
`pkg/controlplane/runtime/shares`, or the block-facing helpers adapters call in
`internal/adapter/common`.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are
to be interpreted as in RFC 2119.

This document specifies what the engine is required to be. It was written from
the model in RFC 0–5 and RFC 8, not from the current package. Where the current
implementation does not satisfy a requirement, that is recorded once, in §12, as
a **deviation**. A deviation is a defect to be fixed or migrated, never a rule
for an implementer to build around.

Where this document chooses a policy the set left open, the choice is labelled
**proposal** and names the measurement that would overturn it.

---

## In short

- The engine holds no bytes and no facts. Every byte is the journal's or the
  remote tier's; every fact is block metadata's or the namespace's.
- It does three things: it **composes** the components, it **decides** policy,
  and it is the **facade** adapters call for content.
- It owns the joins no component can own alone: residency resolution on read,
  block assembly on flush, and the ordering of a write.
- Policy chooses among safe actions. It is never the thing that makes an action
  safe.
- It persists nothing. Losing all of its state costs performance, never content.

---

## 1. Purpose

RFC 0 §1.1 gives the engine *composition, policy, the facade adapters call*, and
nothing else. Every other component answers a question about its own state. The
engine answers the one question none of them can:

> **Given what the two oracles say, what happens next?**

A read has two answers to join. A flush has a carver, a syncer and a metadata
commit to sequence. A full journal has to be told what to evict. A write has four
steps owned by three components. Each of those needs someone who sees all the
parties and belongs to none of them.

### 1.1 Non-goals

The engine **MUST NOT**:

- define a format, an encoding or an algorithm — a record layout, a boundary
  function, a block framing, a key derivation (RFC 1, RFC 2, RFC 8);
- move bytes to or from the remote tier itself — that is the syncer (RFC 3), and
  the engine hands it work;
- hold a copy of any oracle's answer that outlives the operation that asked —
  no residency cache, no durability record, no size (RFC 0 §4.2, RFC 5 §2.5);
- persist anything. Every durable fact is recorded by the component that owns
  it, and a fact the engine persisted would be a third oracle;
- decide when an inode stops existing (RFC 5 §4), or what to delete remotely
  (RFC 7);
- carry namespace features that nothing below the namespace needs to know
  about. The recycle bin RFC 5 §14 asks about is one: it is a rename, block
  metadata never learns of it, and it is not policy over content. It is not the
  engine's.

### 1.2 What it owns

| Concern | What the engine does | Specified in |
| --- | --- | --- |
| Composition | constructs every component of a share and supplies each declared interface | §2 |
| Policy | decides when to flush, what to evict, whether to fill, what to read ahead | §3, §4.2, §6.3, §7 |
| The write | orders authorise, stage, record existence, acknowledge | §4.1 |
| The flush | serialises commits per file, assembles blocks, returns durability | §4, §5 |
| The read | joins the two oracles per missing extent, and answers | §6 |
| Local space | chooses eviction and repack; answers a capacity refusal | §7 |
| Health | derives it from outcomes, including flush outcomes | §8 |
| The facade | one content-shaped surface for every adapter | §9 |

## 2. Composition

### 2.1 The engine is the composition root, and the only one

A share's content path is built in one place, by one constructor, from
configuration and the backends it names. That constructor **MUST** be the only
code that names a concrete component type. Everything else — adapters, the
runtime, other components — holds a declared interface.

The engine is therefore the one package permitted to import the components of
this set, and nothing in the set imports the engine. RFC 0 §1.2's rule is about
the components the engine composes; §11 records the clarification.

Composition **MUST** happen at construction. A capability **MUST NOT** be wired
onto a serving engine by a setter: a setter makes "this capability is absent" a
reachable state of a live share, and every code path then has to decide what to
do in it. Changing a share's backends is a new engine, not a mutation of the old
one.

The engine supplies, at construction:

| Declared by | Need | Supplied from |
| --- | --- | --- |
| RFC 1 | an event recorder (§3.8) | the process's metrics |
| RFC 3 | a put, a verified read (RFC 8 §7) | the remote tier, through the transform chain |
| RFC 4 | nothing | — |
| RFC 5 | `Size(file)` (§2.5), release of an inode's refs (§4.3), allocation for `SEEK` (§9.3) | block metadata's existence record and refs |
| RFC 7 | its narrow views of metadata and the remote tier | block metadata; the remote tier |

### 2.2 Capabilities are parameters, never assertions

Every capability the engine uses **MUST** be a method on an interface it holds by
declaration. A capability that is absent **MUST** fail the build or fail
construction, with an error naming it.

The engine **MUST NOT** negotiate a capability by type assertion, and **MUST
NOT** fall back to a degraded behaviour when one is missing. RFC 0 §1.2 gives the
general reason. Here the consequences are specific: an assertion that fails on
the remote tier disables flushing, one that fails on a sealer uploads plaintext,
one that fails on a lookup makes every cold read linear. Each yields a working
share and no error.

Test fixtures that need less **MUST** supply a stub that satisfies the whole
interface. A production constructor that accepts `nil` for a capability "so tests
can omit it" has made the silent fallback a production path.

### 2.3 A share is one engine

Each share has exactly one engine, one journal and one assembly of policy state.
Backends below the engine — a remote store, a metadata store — **MAY** be shared
between shares and are reference-counted outside it.

Sharing a remote store between shares is subject to RFC 4 §2.6: the engine
**MUST** refuse a composition in which two block-metadata stores can name one
remote key. §5.4 is how it avoids that.

### 2.4 Settings are validated once, and refused rather than replaced

The engine validates every setting it composes — chunking parameters, block
target, pool sizes, capacity — at construction, and **MUST** refuse an invalid
one with an error (RFC 2 §3.7). It **MUST NOT** substitute a default for a value
the operator set.

The engine **MUST** record which chunking settings produced a share's existing
content, and **MUST** report a change to them as a migration rather than apply it
(RFC 2 §3.6). Where the record lives is block metadata's to decide; that it is
consulted at construction is the engine's.

### 2.5 Start in order, stop in reverse, and join before closing

**Start.** Open the journal, which recovers its placement index alone (RFC 1
§9.1). Reseed the journal's flush state from block metadata: every extent that a
carved ref covers is reported durable to the journal, and no other (RFC 1 §9.2).
Until reseeding completes, the engine **MUST NOT** request a release. Only then
start the background policy loops.

**Stop.** Stop accepting operations. Cancel the background loops, then join
them. A component **MUST NOT** be closed while any work that uses it is still
running (RFC 1 §10.7). A join that does not complete within its bound **MUST**
leave the components it depends on open and report the failure, rather than
proceed to close them under a live loop.

> *Note.* A bounded wait followed by teardown is the shape of the "DB closed"
> failure the residency decision record describes: shutdown was on time, and the
> work it abandoned kept running against a closed store.

## 3. Policy

### 3.1 Policy is decided here and executed below

| Decision | The engine decides | The mechanism belongs to |
| --- | --- | --- |
| When to flush a file | eligibility and urgency (§4.2) | journal `Flush` (RFC 1 §3.3) |
| What goes in a block | assembly (§5) | carver, for chunks (RFC 2) |
| When to put a block | as soon as it is assembled | syncer uploader (RFC 3 §3.1) |
| Whether to fill | §6.3 | journal `Fill` (RFC 1 §3.4) |
| What to read ahead, pre-warm | §6.4 | syncer fetcher (RFC 3 §4.5) |
| What to evict, and when | §7.1 | journal `Release` (RFC 1 §3.5) |
| When to repack | §7.3 | journal repack (RFC 1 §8.2) |
| Whether to keep accepting writes | §8.2 | journal capacity (RFC 1 §7) |
| What follows from ill health | §8.1 | — |

A component that takes one of these decisions itself has absorbed engine policy,
and a threshold that lives in a component's configuration is such a decision.

### 3.2 Policy never makes an action safe

Every mechanism the engine calls is safe by its own definition: `Release` refuses
an extent whose flush bit is unset (RFC 1 §3.5), `Fill` refuses to overwrite held
bytes (RFC 1 §3.4), a flush bit is set only on a durability report (RFC 1 §3.3).
Policy chooses *among* safe actions.

A policy gate **MUST NOT** be the only thing standing between the system and data
loss. If turning a gate off would lose content, the gate is carrying a safety
property that belongs to the mechanism, and the mechanism is wrong.

> *Note.* The distinction is testable. Disable every policy gate at once — evict
> as eagerly as possible, fill everything, flush constantly — and run the Group A
> checks of §13. A conformant engine is slower and still correct.

### 3.3 Policy state is memory, and disposable

Access patterns, dirty ages, flush backoff, readahead frontiers and derived health
**MUST** live in memory. A restart **MAY** lose all of it, and the engine
**MUST** behave correctly from an empty policy state — conservatively, never
unsafely.

## 4. The write and the flush

### 4.1 The facade orders a write; adapters do not

A write is one facade operation. The engine performs, in this order:

1. authorise the write against the namespace (RFC 5 §7);
2. stage the bytes in the journal (RFC 1 §3.1);
3. record existence — `size` grown, holes shrunk, `mtime` and `ctime` in the same
   transaction (RFC 4 §3.4, RFC 5 §2.5);
4. acknowledge.

No adapter **MAY** perform these steps itself or in another order. A sequence
copied into each protocol handler is a sequence each handler can get wrong
differently, and the one that gets it wrong serves zeros for acknowledged data
(RFC 4 §3.1).

The engine **MAY** group-commit step 3 across writes, and **MUST NOT**
acknowledge any write in a group before the group commits (RFC 4 §3.4).

### 4.2 Flush is scheduled here

A file becomes eligible for a flush pass when any of these holds:

- its dirty bytes reach the block target;
- its oldest dirty byte reaches a configured maximum age;
- the journal is under capacity pressure (§7.2);
- a client asked for durability the configured acknowledgement policy defers to
  the remote (§9.4).

**Proposal:** a dirty-byte threshold of one block target and a maximum age of a
few seconds, with capacity pressure making every file with dirty bytes eligible.
Overturned by a measurement showing the age bound, not the byte bound, is what
fragments blocks under a streaming SMB workload.

A failed pass leaves its extents **Dirty** and is retried (RFC 0 §5.2). Retries
back off with jitter; they **MUST NOT** stop, and a failing pass **MUST** be
reported to health (§8.1).

### 4.3 Commits for one file are serialised here

RFC 4 §4.4 leaves the choice of mechanism to the engine. The engine **MUST NOT**
run two flush passes of one file concurrently: it holds a per-file guard from the
moment the journal offers a run until the callback returns its durable extents.

The guard **SHOULD** be keyed by file. A guard keyed by a shard or a stripe is
correct and serialises unrelated files that collide on it, which makes one slow
file's commits every colliding file's latency. The guard **MUST NOT** be held
across anything but the pass it serialises.

Blocks within one pass cover disjoint offsets, so their commits **MAY** run
concurrently; what the guard protects is the order *between* passes.

### 4.4 The truncation epoch is captured at offer and checked at commit

When the journal offers a run, the engine reads the file's `epoch` (RFC 4 §6.2)
and carries it into every commit that pass makes. A commit whose epoch no longer
matches is refused by block metadata; the engine treats that refusal as a failed
pass and re-offers from the journal, which has already truncated (RFC 1 §3.6).

### 4.5 The callback returns only what committed

The flush callback **MUST** return exactly the extents whose commits succeeded,
in the order the journal offered them, and **MUST** return a committed prefix
together with the error that stopped the pass (RFC 0 §5.2, RFC 1 §3.3). An extent
whose put succeeded and whose commit did not is not durable, and **MUST NOT** be
returned (RFC 4 §4.3).

A block may carry chunks from several offered runs. Its commit makes all of them
durable at once, and the callback reports each run's share of it when that run's
turn in the order comes.

### 4.6 A share with no remote tier never reports durability

On a share with no remote tier nothing is ever durable remotely, so the flush
callback **MUST** return nothing, and no flush bit is ever set (RFC 4 §4.2). The
content stays **Dirty** for its lifetime, and eviction has nothing to act on —
by definition, not by a gate.

Such a share still syncs its journal on commit (§9.4). It does not carve, and it
writes no chunk, block or ref records. Operations that need refs — clone,
snapshot — copy bytes on such a share (§9.3).

## 5. Block assembly

### 5.1 Blocks are assembled here, as a fold over the carver's output

RFC 2 §5 places block assembly with whoever owns the dedup query, and that is the
engine. Assembly is a fold over the chunks the carver emits: for each chunk, ask
the dedup oracle (§5.3), then either carry its bytes into the pending block or
record it as adopted.

The fold **MUST** obey RFC 2's packing rules: whole chunks only (P1); a block
reaches its target and overshoots it by at most one chunk (P2); a block holds
only the chunks whose bytes it carries (P3). The file's refs are a different
reader of the same chunk sequence and name every chunk, carried or adopted.

The carver's bytes are borrowed for the length of `emit` (RFC 2 §2.2). The engine
copies a carried chunk into the pending block's buffer once, and that buffer is
the block's for its whole life — framed, put and released when the commit
returns. The number of pending and in-flight block buffers is bounded by the
upload pool (RFC 3 §2.2); an assembler that allocates ahead of the pool has
moved the memory bound somewhere nobody stated.

An assembler is per file and per pass. One **MUST NOT** be shared between two
files or survive its pass: it would interleave two files' chunks into one block.

### 5.2 The target counts carried bytes

P2's target is measured in the bytes a block carries. Adopted chunks contribute
nothing to it. An assembler that counts every chunk it tiles emits a block when a
pass has *seen* a target's worth of content, which on a mostly-deduplicated file
is a block of a few kilobytes.

### 5.3 The dedup oracle never sees an uncommitted block

The oracle is RFC 4 §8.2's `Durable(hash)`: a chunk record exists, so its block
is durable. It **MUST NOT** answer from anything that knows about a block not yet
committed — the pending block, a block in flight, a put that succeeded and whose
commit has not (RFC 2 §5).

Two refinements follow, and both are required:

- **Within one pending block**, a repeated chunk **MAY** be carried once and
  referenced twice. Both refs and the bytes commit in one transaction, so they
  cannot be separated by a failure.
- **Across blocks in flight**, a repeated chunk **MUST** be carried again. The
  earlier block may fail after the later one commits, and the later one's refs
  would then name bytes that exist nowhere.

The oracle's answer is advisory. A chunk it reports may be retired before the
adopting commit applies; that commit then fails (RFC 4 §7.2), and the engine
**MUST** re-offer the run with that chunk carried. The engine **MUST NOT** hold
anything — a lock, a reservation, an in-process guard — to make the answer
binding. A guard another process cannot see protects nothing across processes,
and RFC 4 §7.2 makes one unnecessary within a process.

### 5.4 A block's name is derived here

The engine assembles the block, so it computes the name (RFC 2 §4): a hash, under
a domain distinct from chunk hashing, of the key scope and the block's chunk
hashes in order. The name is final before framing begins (RFC 8 §3.4) and does
not vary between attempts to store one block, so a retry after an unknown outcome
writes the same object (RFC 3 §2.5).

**Proposal — the key scope is the identity of the block-metadata store that
counts the block.** This is RFC 4 §2.6's second option: two stores can never name
one object, so each store's counts are complete for every key it can name, and a
remote store can be shared between shares whose metadata is separate. The cost is
dedup across such shares, which today's per-share metadata stores do not provide
safely anyway. Overturned by a deployment that wants cross-share dedup *and*
accepts one metadata store per remote namespace.

### 5.5 Assembly is sequential, and may change without migration

**Proposal:** chunks are assembled into blocks in file-offset order, which keeps
a sequential read's chunks in few blocks. Randomised assembly (RFC 2 §6) blurs the
chunk-size fingerprint an observer of object sizes could use, costs nothing in
dedup, and — because names derive from content and refs name hashes (RFC 4 §2.5)
— can be adopted later with no migration. It is left open (§14).

### 5.6 A run is what the journal offers, widened only to re-tile

The engine offers each maximal dirty stretch the journal holds as one carver call,
and never joins two stretches: a chunk must not straddle a hole (RFC 2 §2.1).

A longer stretch dedups better and is the caller's lever. The engine **MAY** widen
a run over contiguous bytes the journal holds and that are already durable, but
**only** so the new chunking re-tiles a ref the run partially replaces. Widening
for dedup alone re-uploads content that is already remote.

## 6. The read

### 6.1 Resolution is a join, computed per request

![One read: held bytes from the journal, each missing extent classified by metadata as hole, uncarved or carved, the carved ones fetched, the reply taken from the verified bytes, and the fill as a separate dashed decision](img/rfc6-read-resolution.svg)

For a read of `(file, off, len)`, the engine:

1. asks the journal, and receives the bytes it holds and the exact extents it does
   not (RFC 1 §3.2);
2. for each missing extent, asks block metadata which class covers it (RFC 4
   §8.1), using the range form where one is available;
3. resolves each part by RFC 0 §4.2 as amended by RFC 4 §3.2:

| Metadata | Residency | The engine |
| --- | --- | --- |
| hole | **Absent** | returns zeros |
| uncarved | **Lost** | fails the read, and reports data loss naming the file and extent |
| carved | **Remote** | gets the block's chunk, verified (RFC 3 §4.1) |
| past end of file | — | returns a short read |

The engine **MUST** compute this per request and **MUST NOT** keep its result,
nor any structure from which it would answer a later read without asking both
oracles (RFC 0 §4.2). It **MUST NOT** return zeros for any part except a hole.

A fetch is issued per missing extent, never per window: an extent the journal
holds is not fetched because a neighbour was not held (RFC 1 §3.2).

### 6.2 The reply is served from the fetched bytes

The engine answers the read from the verified bytes the fetch returned (RFC 3
§4.1). It **MUST NOT** answer by re-reading the journal after a fill: that makes
the reply wait on the fill, and makes a fill failure — a full journal, a local I/O
error — fail a read whose bytes were correct in hand (RFC 3 §4.2).

When it does fill, the engine passes `Fill` exactly the bytes it fetched, at the
offsets of the extent it resolved, together with the journal version it read
before resolving, so a write that landed meanwhile wins (RFC 1 §3.4, RFC 0 I4).

### 6.3 Filling is a decision

RFC 0 §6.2 makes filling discretionary and gives the decision to the engine.

**Proposal — fill a demanded extent unless one of these holds:**

- the journal's free capacity is below a low-water mark reserved for writes, so
  that a fill never pushes a write towards refusal;
- the read is part of a sequential scan already longer than the readahead window,
  where retaining what was just read displaces content more likely to be read
  again;
- the fetch served a pre-warm that has been asked to yield (§6.4).

A declined fill still answers the read. Speculative fetches follow the same rule
with the low-water mark applied more strictly than for demand.

This settles RFC 0 open question 2 as a proposal only. What decides it is a
hit-rate and write-refusal measurement on the large-file SMB workload, comparing
fill-always, the rule above, and fill-never.

### 6.4 Speculation is planned here, and yields to demand and to writes

The engine sees the access pattern and the free capacity, so it decides what to
fetch before it is asked for (RFC 3 §4.5). It issues those fetches to the fetcher
as speculative, which the fetcher never lets delay a demand (RFC 3 §4.4).

**Read-ahead** follows an observed sequential pattern, a window of blocks ahead of
the reader. A random access resets it. Its window **MUST** be bounded in bytes,
and the bound is subtracted from the capacity a fill may use.

**Pre-warm** is an explicit request over a whole share or subtree. It **MUST NOT**
drive the journal towards refusing writes (RFC 3 §4.5). **Proposal** for how it
yields (RFC 3 open question 3): pre-warm fills only while free capacity is above
the write low-water mark, pauses when capacity falls below it, and cancels its
queued fetches on a write that meets capacity pressure. A pre-warm that stops
early reports how far it got; it is re-issuable, so stopping costs nothing.
Overturned by a measurement showing the pause-and-resume churn costs more than a
fixed reservation would.

### 6.5 An unreachable remote fails the read, distinguishably

A **Remote** extent whose fetch cannot complete — the remote is unreachable, or
the demand deadline expires — **MUST** fail the read with an error distinguishable
from **Lost** (RFC 0 §10). The first is transient and the client may retry; the
second is data loss and must be reported as such.

### 6.6 Allocation answers from the hole set

`SEEK_DATA`, `SEEK_HOLE` and sparse-read replies are answered from block
metadata's hole set, through the allocation interface the engine supplies to RFC 5
(§9.3 there). They **MUST NOT** be answered from the journal, which cannot tell a
hole from an evicted extent (RFC 1 §3.7).

## 7. Local space

### 7.1 Eviction is chosen here, and needs no new record

The engine selects what to evict and calls `Release` on it. **Proposal:** coldest
first by last access, in units the journal can free (RFC 1 §8.1), until a target
set by capacity pressure is met.

RFC 1 §3.5 requires the caller to have durably recorded that content is no longer
local before releasing it. Under the model that record already exists and is the
flush commit: a flush bit is set only after the ref, chunk and block are committed
(RFC 4 §4.3), and a carved extent the journal does not hold resolves to
**Remote**. Eviction therefore writes nothing to metadata. §11 records the
consequence for RFC 0 §8.1's wording.

### 7.2 A capacity refusal comes back here

The journal refuses a write it cannot reserve for, and does not evict for itself
(RFC 1 §7). The engine answers the refusal, per RFC 0 §10:

1. evict (§7.1), then retry;
2. if nothing is evictable, repack (§7.3), then retry;
3. if neither frees space, refuse the write with a distinguishable error.

The retry is bounded by the caller's deadline. A wait that outlives it is a
refusal the client did not get.

### 7.3 Repack is triggered here

The engine requests a repack when the journal's statistics show storage it can
recover: allocated storage well above held bytes (RFC 1 §8.3), a descriptor count
at its bound (RFC 1 §8.4), or an extent count past its bound (RFC 1 §5.2). It **MUST** request
one when the journal is at capacity and nothing is evictable, because repack's
reserved headroom exists for exactly that (RFC 1 §8.2).

### 7.4 Nothing but durability makes an extent unevictable

Locks, deny modes, delegations and open handles **MUST NOT** make an extent
ineligible for eviction (RFC 5 §8.6). Neither **MAY** a snapshot: a snapshot holds
counted refs (RFC 4 §6.5), and its content is as evictable as any carved content.

An operator's retention pin **MAY** exclude a share from eviction, and the engine
**MAY** suspend eviction while the remote is unreachable, because evicting then
turns a readable extent into one that fails until the remote returns. Both are
availability policy. Neither is permitted to be what keeps content safe (§3.2).

## 8. Health and failure

### 8.1 Health is derived from recent outcomes, flush included

Share health **MUST** be computed from recent outcomes and **MUST NOT** be a
stored flag that suppresses the attempts that would clear it (RFC 3 §5). Its
inputs include the outcomes of flush passes, not only the remote's liveness probe.

Sustained inability to flush **MUST** be a health condition of the share (RFC 0
§10.2), and **MUST** be distinguishable from the remote being unreachable: a flush
that fails on a metadata conflict with the remote healthy is the wedge the
residency decision record describes, and it looks healthy to a probe.

What the engine does with ill health is policy: it **MAY** slow flush attempts and
suspend eviction. It **MUST NOT** stop attempting altogether without something
independent — a probe — that will observe recovery.

### 8.2 Every condition in RFC 0 §10 has its engine behaviour here

| Condition | The engine |
| --- | --- |
| Remote unavailable | keeps accepting writes while capacity allows; keeps retrying flushes with backoff; fails reads of **Remote** extents (§6.5); reports degraded health |
| Journal at capacity, remote available | evicts, then repacks, then accepts (§7.2) |
| Journal at capacity, remote unavailable | refuses the write; everything held is **Dirty** |
| Metadata unwritable | flush fails and extents stay **Dirty**; reports the flush condition (§8.1) |
| Crash | recovers each component independently, then reseeds (§2.5); does not reconcile the oracles by assuming they agree |
| Local content corrupt | a carved extent the journal dropped resolves **Remote** and is refetched; an uncarved one resolves **Lost** and fails (RFC 1 §9.3) |

### 8.3 The engine surfaces no serialization conflict

The engine is a caller of metadata transactions — existence on the write path,
commits on flush. A conflict **MUST** be retried under the caller's deadline and
**MUST NOT** reach a client as an I/O error (RFC 0 I8). A flush commit's caller is
the background pass, whose deadline is the pass's own; a conflict there costs a
retry, not a failed pass.

The per-file guard of §4.3 reduces flush-against-flush conflicts. It **MUST NOT**
be relied on for correctness: RFC 4 §5.1 removes flush-against-writer conflicts
structurally, and a conflict the guard missed is retried like any other.

## 9. The facade

### 9.1 One facade, shaped like content

Adapters reach content through one surface, and never through a component:

| Operation | Composes |
| --- | --- |
| `Write(file, off, bytes)` | §4.1 |
| `Read(file, off, len)` | §6 |
| `Commit(file)` | journal sync, and a flush where the acknowledgement policy requires it (§9.4) |
| `Truncate(file, size)` | RFC 4 §6.2 in one transaction, then journal `Truncate` |
| `Deallocate(file, off, len)` | §9.2 |
| `Release(file)` | RFC 4 §6.4 refs, then journal `Delete` — the implementation of RFC 5 §4.3 |
| `Clone(src, dst, …)` | §9.3 |
| `Size`, `Allocation` | the interfaces of §2.1, read-only |
| `Stats`, `Health` | §8 |

The facade **MUST NOT** return a component — a journal, a remote store — to its
caller. A caller holding one can do what the facade orders, out of order.

### 9.2 Deallocate records a hole; it does not write zeros

Deallocation makes the range a hole in the existence record, drops or narrows the
refs over it and advances `epoch`, in one transaction (RFC 4 §3.5, §6.2), then has
the journal stop holding the range.

It **MUST NOT** stage zeros through the write path. Zeros staged as data consume
journal capacity in proportion to the range — a large deallocation can refuse
writes — and carve into the hottest refcount in any deployment (RFC 4 §5.3).

### 9.3 Clone adopts refs, and flushes uncarved content first

On a share with a remote tier, a clone flushes the source's uncarved extents,
then copies the source's refs into the destination as an adoption (RFC 4 §6.6).
**Proposal:** flush first rather than copy bytes, because it leaves one path for
clone and makes the destination's content shared from its first byte. On a share
with no remote tier nothing is carved (§4.6), so a clone copies bytes through the
destination's write path.

### 9.4 Commit's acknowledgement is a stated policy

The facade acknowledges `Commit` when the content is recoverable under the share's
configured policy: journal-durable by default, remote-durable for a share that
requires it. The policy is per share and is stated in its configuration; the
engine **MUST NOT** report a stronger durability than the one it waited for.

### 9.5 The facade writes no residency

The facade **MUST NOT** offer an operation that tells the journal an extent is
remote, cold, or pinned. Residency is computed (RFC 0 §4.2). An operation that
records it is a second oracle, and every such operation in the current facade
exists only because the journal once was one (§12).

## 10. Invariants

| # | Invariant |
| --- | --- |
| E1 | The engine persists nothing, and no answer it gives outlives the request that asked. |
| E2 | Every capability is a declared parameter, supplied at construction; none is negotiated at run time. |
| E3 | No policy gate is the only thing preventing data loss. |
| E4 | A flush reports exactly the extents whose commits succeeded, in offer order. |
| E5 | A share with no remote tier reports nothing durable. |
| E6 | Two flush passes of one file never overlap, and a commit carries the epoch captured at offer. |
| E7 | The dedup oracle never answers from a block that has not committed, and a chunk repeated across blocks in flight is carried in each. |
| E8 | A block's name is derived from its scope and ordered chunk hashes, and is the same on every attempt. |
| E9 | A read returns zeros only for a hole, and fails distinguishably for **Lost** and for an unreachable remote. |
| E10 | A read's reply never depends on the fill. |
| E11 | Only durability decides whether an extent may be evicted; locks, opens and snapshots never do. |
| E12 | Sustained flush failure is a health condition, distinct from an unreachable remote. |
| E13 | No background work outlives a component it uses. |

E3, E4, E5, E7, E9 and E11 are the ones whose violation loses content or serves
wrong content. E10, E12 and E13 are the ones whose violation stops a share, or
makes it look stopped. E1 and E2 are the ones whose violation hides the others.

## 11. Consequences for RFC 0

1. **§1.2 and I6.** "A component MUST NOT import another component in this set"
   cannot hold for the component whose job is composition. It holds for every
   component the engine composes; the engine imports them and nothing imports the
   engine (§2.1). I6 should say so, or it reads as forbidding the root.
2. **§8.1, the order of eviction.** "Record that the content is no longer local,
   then release" reads as a metadata write about locality, which §4.1 forbids
   metadata to hold. Under RFC 4 §3.2 the record that makes release safe is the
   flush commit, made before the flush bit was set, and eviction records nothing
   (§7.1). The ordering argument is unchanged; the record it names is a different
   one.
3. **§6.2 and open question 2.** Fill policy is specified, as a proposal, in §6.3.
   The question's measurement stands.

## 12. Deviations

The current implementation was checked against this document after it was
written. The design above does not follow from any of what is listed here.
Rows already recorded by another RFC are cited there rather than restated, except
where the engine is the site that must change.

### 12.1 Composition

| Requirement | Current state | Evidence |
| --- | --- | --- |
| §2.1 one composition root | Composition is split between the runtime, which opens the journal, builds the remote chain and the syncer, and `engine.New`, which receives them. The runtime names concrete component types throughout. | `runtime/shares/blockstore_config.go:251`–`:447`; `engine/engine.go:152` |
| §2.1 no setters on a serving engine | The remote block store, the committer and the metrics sink are wired by setters after construction, and the code documents that they may run on a serving share. | `engine/syncer.go:235`, `:259`; `engine/engine.go:354`; `engine/sync_drain.go:49`–`:59` |
| §2.2 no type assertions | Capabilities are negotiated by assertion, each with a silent fallback, at fifteen sites: the remote block store (flush disabled), the metadata coordinator and synced-hash store, the committer (flush disabled), the chunk sealer (identity sealing), the metrics sink, four optional sink capabilities in the flush closure, the stale-claim enumerator (janitor becomes a no-op), and the covering and successor lookups (RFC 4 §11 records their cost). | `runtime/shares/blockstore_config.go:335`, `:355`, `:371`; `engine/syncer.go:244`, `:263`; `engine/engine.go:355`, `:360`; `engine/flush_closure.go:188`, `:230`, `:234`, `:314`; `engine/sync_lifecycle.go:116`; `engine/read_internal.go:217`, `:289`, `:354` |
| §2.4 settings refused | Invalid chunking settings are replaced by the default profile in three places, one with a warning and two silently. No record of the profile that produced a share's content was found by search. | `runtime/shares/journal_open.go:95`–`:99`; `engine/sync_drain.go:110`–`:113`; `carver/carver.go:91`–`:93` |
| §2.5 reseed before release | There is no reseed. The flush bit is written into each record's header, the syncer's start path states that recovery re-marks only not-yet-carved records dirty, and eviction is enabled at start from remote health alone. | `journal/flush.go:244`–`:257`; `engine/sync_lifecycle.go:53`–`:55`; `engine/engine.go:247` |
| §2.5 join before close | The syncer waits a bounded time for its loops, logs if they have not exited, and the engine then closes the journal and the remote under them. | `engine/sync_lifecycle.go:201`–`:203`; `engine/engine.go:332`–`:342` |
| §9.1 no component returned | The facade returns its journal and its remote store to callers. | `engine/engine.go:368`, `:422` |

### 12.2 Durability and policy placement

| Requirement | Current state | Evidence |
| --- | --- | --- |
| §4.6 no durability without a remote | On a share with no remote tier the flush closure commits manifest rows through a local sink and returns the committed extents, and the journal marks them durable. What keeps them from being evicted is the engine's eviction gate: carve not wired, so eviction stays suspended. That is §3.2's forbidden shape — the gate is the only thing between those extents and **Lost**. | `engine/flush.go:238`–`:243`; `engine/flush_closure.go:167`–`:174`; `journal/flush.go:183`; `engine/sync_health.go:218`–`:220`; `engine/engine.go:212`–`:247` |
| §3.1, §4.2 flush policy here | The eligibility thresholds — block size and maximum age — are journal configuration, applied inside the journal's `Flush` when the caller passes none, which the background dispatcher does. | `journal/flush.go:143`–`:152`; `journal/store.go:50`–`:51`, `:112`–`:113`; `engine/carve_dispatch.go:150` (the dispatcher passes no thresholds) |
| §4.2, §8.1 flush retried and never stopped | While the remote is unhealthy the dispatcher skips every pass, and an explicit flush returns "not finalized". Only the liveness probe can clear it. | `engine/carve_dispatch.go:45`; `engine/sync_drain.go:64`–`:70` |
| §3.1, §7.1, §7.2 eviction and refusal here | The journal evicts to satisfy its own write: its capacity gate selects coldest-first segments, evicts, backpressures and finally refuses — none of it through the engine. Admission reads a counter without reserving (the code says so). | `journal/evict.go:166`, `:453`–`:539` |
| §4.3 the engine's guard, per file | The outcome holds: passes of one file do not overlap. But the guard is the journal's shard-scoped flush lock, held across the callback, and commits within a pass take a 256-stripe lock in the engine keyed by a hash of the file id. Both serialise unrelated files that collide, and the first puts the engine's decision inside the journal. | `journal/flush.go:98`–`:100`, `:118`–`:120`; `engine/flush.go:115`–`:135` |
| §4.4 epoch | No epoch is captured or checked; the existence record it lives in does not exist (RFC 4 §11). | `engine/flush_closure.go` (no epoch in the closure) |
| §8.1 flush in health | A failed pass increments a lifetime counter and logs a warning. Engine health is the local store's closed flag and the remote's probe; share health is the worst of engine and metadata. Flush failure reaches neither. | `engine/carve_dispatch.go:152`–`:153`; `engine/health.go:41`–`:79`; `runtime/shares/healthcheck.go:59`–`:95` |

### 12.3 Assembly

| Requirement | Current state | Evidence |
| --- | --- | --- |
| §5.1 assembly in the engine | Block assembly — the pending batch, the target, emission — is inside the carver, which RFC 2 §1.1 forbids. | `carver/carver.go:62`–`:84`, `:175`–`:182`, `:195`–`:217` |
| §5.2 target counts carried bytes | The batch counts every chunk it tiles, adopted ones included, and emits when that count reaches the target. | `carver/carver.go:176`, `:180` |
| §5.3 no binding guard | The oracle answers from the synced-hash marker (RFC 4 §11) through an in-process adoption guard in the GC package, which a second process cannot see. | `engine/flush.go:148`–`:153` |
| §5.4 derived name | The name is sixteen random bytes; a separate hash is taken over the framed block bytes. | `engine/flush.go:392`, `:436` |
| §5.1 one buffer | A carried chunk is copied into the carver's arena, the run is read through a separate 16 MiB buffer per run, and the framed block is built in a third buffer. | `carver/carver.go:223`–`:237`; `engine/flush_closure.go:112`; `engine/flush.go:397`–`:403` |

### 12.4 The read

| Requirement | Current state | Evidence |
| --- | --- | --- |
| §6.1 per missing extent | The journal reports two booleans for the whole window, not extents, so the engine fetches every covering chunk of the window when any byte is missing. | `journal/index.go:293`–`:296`; `engine/read_internal.go:56`–`:64`; `engine/fetch.go:491`–`:499` |
| §6.1 hole vs uncarved | With no existence record, a missing extent no ref covers is served as zeros whatever its cause, and on a share with no remote tier a missing extent is never looked up at all. | `engine/read_internal.go:56` |
| §6.2 reply independent of fill | Every demanded fetch fills, and the read is answered by re-reading the journal afterwards. A fill failure fails the fetch, and a window still unfilled after two tries fails the read. | `engine/fetch.go:669`–`:674`; `engine/read_internal.go:117`–`:131` |
| §6.3 fill is a decision | There is no fill policy: every demanded and every read-ahead fetch fills. Read-ahead keeps 64 blocks ahead of a sequential reader. | `engine/fetch.go:460`, `:669`; `engine/types.go:56`; `engine/readahead.go:80`–`:89` |
| §6.4 pre-warm yields | Warm fetches every chunk of every file until done, cancelled, or the journal refuses on capacity, which ends the run. | `engine/warm.go:60`–`:63`, `:184`–`:186` |
| RFC 3 §2.1 one bound per half | Each cold read and each warm run builds its own fetch group bounded at the configured parallelism; the read-ahead pool is a third. Total fetches in flight scale with concurrent readers. | `engine/fetch.go:31`–`:39`, `:540`; `engine/warm.go:175`; `engine/sync_queue.go:89`–`:92` |
| §6.6 allocation from the hole set | `SEEK` is answered from the journal's extents joined with the manifest rows. | `engine/dataextents.go:61`, `:84` |
| E1 no dead state | An in-memory read cache is configured and started, but nothing on the read path consults it and its only loader always misses, so it is never populated. | `engine/cache.go:46`–`:51`, `:425`; `engine/engine.go:256`–`:275` |

### 12.5 The facade

| Requirement | Current state | Evidence |
| --- | --- | --- |
| §4.1 the facade orders a write | The facade's write stages bytes only. Authorise and existence are called by each protocol handler around it, at five sites, and existence may be deferred in memory past the acknowledgement (RFC 5 §12.1). | `internal/adapter/common/write_payload.go:54`–`:69`; `nfs/v3/handlers/write.go:235`, `:266`; `PrepareWrite` also in `nfs/v4`, `smb/handlers/write.go`, `ioctl_copychunk.go`, `ioctl_sparse.go` |
| §9.2 deallocate records a hole | Deallocation writes zeros through the journal in 1 MiB pieces across the whole range. | `engine/readwrite.go:330`–`:344` |
| §9.1 truncate in one transaction | Truncate narrows a straddling row with a store write outside any transaction, then decrements and reaps in a second call, reprojects in a third, then truncates the journal. | `engine/readwrite.go:151`–`:152`, `:233`, `:239`, `:245` |
| §9.5 no residency writes | The facade marks ranges remote-but-not-local, drops local content to force manifest reads, pins journal versions for snapshots, and rewinds the journal to a version. | `engine/flush.go:639`–`:765` |
| RFC 8 §2.5 | The fetch path, derived health, the read-ahead queue and the put live in this package. RFC 8 records it; the move is this package's. | RFC 8 §2.5 |

This document does not schedule the migration. It records that the current state
fails the requirements above, and that a discrepancy **MUST NOT** be closed by
amending the requirement.

## 13. Conformance

RFC 1 §11 applies unchanged: conformance is every **MUST** holding, a check is
evidence for a requirement that fails silently, and a check is validated by
reverting the code and watching it fail on its own assertion. Every check here
runs against the engine as production composes it (RFC 1 §11.5).

### 13.1 Group A — lost or wrong content

| Requirement | Check |
| --- | --- |
| §3.2 no gate is safety | Disable suspension, pins and health gating; evict as eagerly as possible; run every other Group A check. Assert all pass. |
| §4.6 local-only | On a share with no remote, flush repeatedly and force eviction. Assert no flush bit is set and every read returns its bytes. |
| §4.5 committed prefix | Fail the second of three block commits. Assert the journal marks exactly the first block's extents. |
| §5.3 in-flight dedup | Carve a chunk into two blocks in flight; fail the first put after the second commits. Assert the second block carries the chunk's bytes and a read succeeds. |
| §5.3 retired adoption | Retire a chunk between the oracle's answer and the commit. Assert the commit fails, the run is re-offered carrying the chunk, and the read succeeds. |
| §6.1 the join | Drive all four rows of §6.1, including an uncarved extent the journal lost. Assert **Lost** fails — a check of the other three passes a build that serves zeros. |
| §6.2 fill cannot fail a read | Make `Fill` fail. Assert the read returns the fetched bytes. |
| §6.2 fill loses to a write | Stall a fetch; write the extent; release the stall. Assert the written bytes survive. |
| §9.2 deallocate | Deallocate a range larger than free journal capacity. Assert it succeeds, reads as zeros, and consumes no journal capacity. |

### 13.2 Group B — wedging

| Requirement | Check |
| --- | --- |
| §8.1 flush health | Make every flush commit conflict with the remote healthy. Assert share health reports a flush condition, distinct from remote-unreachable, before the journal fills. |
| §7.2 refusal loop | Fill to capacity with durable content. Assert a write succeeds after the engine evicts, with no external action. |
| §6.4 pre-warm yields | Pre-warm more than free capacity while writing. Assert no write is refused. |
| §2.5 join | Close with a flush parked in a stalled put. Assert no component is closed while the pass runs. |
| RFC 3 §2.1 | Issue N concurrent cold reads. Assert fetches in flight never exceed the configured pool. |

### 13.3 Group C — composition

| Requirement | Check |
| --- | --- |
| §2.2 no assertions | Remove one method from each capability's provider. Assert the build fails, not the behaviour. |
| §2.4 settings | Configure `Min ≥ Target`. Assert construction fails. Change a share's profile. Assert it is reported as a migration. |
| §2.5 reseed | Restart with carved content held locally. Assert no release is requested before reseeding and every reseeded extent is evictable after. |

### 13.4 What must not stand in

- **A sink that always succeeds MUST NOT be used for Group A.** §4.5 and §5.3 are
  about what the engine reports when a commit or a put fails.
- **A single-file rig MUST NOT stand in for §4.3 or §5.3.** Both need two passes
  or two blocks in flight at once.
- **A health check that reads the remote probe MUST NOT stand in for §8.1.** The
  failure it exists for passes the probe.

## 14. Open questions

1. **Fill policy parameters** (§6.3). The rule is a proposal; its low-water mark
   and scan threshold are unmeasured, and so is whether fill-never on large scans
   costs more re-fetches than it saves capacity.
2. **Pre-warm's yield mechanism** (§6.4). Pause-and-cancel is proposed over a
   fixed reservation; they behave differently when a write burst arrives mid-warm,
   and neither has been run.
3. **Randomised assembly** (§5.5, RFC 2 §10 question 4). Free to adopt and not a migration.
   What an observer can recover from object sizes at DittoFS's block sizes has
   not been measured, so neither has the value of adopting it.
4. **Zero runs** (RFC 4 §13.1). Recording an all-zero chunk as a hole removes the
   hottest refcount in the system. The engine sees the chunks before assembly and
   could do it, but it changes existence from the flush path, which RFC 4 §5.1
   forbids. Where the recognition belongs is unsettled.
5. **Copies on the flush path** (§5.1, RFC 2 §10 question 5). The requirement is one copy
   into the block buffer; whether framing and sealing can write in place over it,
   and what that saves on the production CPU, is unmeasured.
6. **Key scope** (§5.4). The proposal forgoes cross-share dedup. Whether any
   deployment wants it enough to accept one metadata store per remote namespace
   is a product question, not a measurement.
7. **Flush thresholds** (§4.2). The proposed age and byte bounds are the
   historical defaults, not measured ones.

---

## Appendix A — obligations this document discharges

Every sentence in the set that defers to RFC 6 or to "the engine", and every
obligation placed on "the caller" of a component where the engine is the only
caller. *Explicit* rows name RFC 6 or the engine; *caller* rows name the caller.

| Source | Obligation | Kind | Discharged in |
| --- | --- | --- | --- |
| RFC 0 §1.2 | supply each component's declared interfaces at composition | explicit | §2.1, §2.2 |
| RFC 0 §4.1 | resolve what the journal's absence means | explicit | §6.1 |
| RFC 0 §4.2 | never persist or cache resolved residency | caller | §6.1, E1 |
| RFC 0 §5.2 | initiate flush by policy | caller | §4.2 |
| RFC 0 §6.2, §11 q2 | specify a fill policy | explicit | §6.3 — proposal; open (§14.1) |
| RFC 0 §8.1 | evict in the safe order | caller | §7.1, §11.2 |
| RFC 0 §8.2 | drive reclaim | caller | §7.3 |
| RFC 0 §10 | one behaviour per failure condition | caller | §8.2 |
| RFC 0 §10.2 | sustained flush failure is share health | caller | §8.1 |
| RFC 0 §9.2 (I8) | surface no conflict | caller | §8.3 |
| RFC 1 §2, §3.2 | resolve hole, evicted, lost | explicit | §6.1 |
| RFC 1 §3.4 | fill only fetched bytes for that extent, never superseded | caller | §6.2 |
| RFC 1 §3.5 | eviction policy; durable record before release | explicit | §7.1 |
| RFC 1 §3.7 | combine `Extents` with metadata for `SEEK` | caller | §6.6 |
| RFC 1 §3.8 | supply a recorder at construction | caller | §2.1 |
| RFC 1 §7 | evict and retry on a refused reservation | explicit | §7.2 |
| RFC 1 §8 | reclamation policy | explicit | §7.3 |
| RFC 1 §9.2 | reseed flush bits from metadata before enabling eviction | explicit | §2.5 |
| RFC 2 §2.1 | one call per stretch; longer stretches are the caller's lever | caller | §5.6 |
| RFC 2 §2.2 | copy borrowed bytes | caller | §5.1 |
| RFC 2 §3.6, §3.7 | record the profile; refuse bad settings; report changes as migrations | caller | §2.4 |
| RFC 2 §4 | compute a block's identity | explicit | §5.4 |
| RFC 2 §4.3 | choose the key scope explicitly | caller | §5.4 — proposal; open (§14.6) |
| RFC 2 §5 | build blocks under P1–P3 | explicit | §5.1, §5.2 |
| RFC 2 §5 | the oracle hazard | explicit | §5.3 |
| RFC 2 §6 | randomised assembly | explicit | §5.5 — open (§14.3) |
| RFC 2 §8 | invariants for assembly against the oracle | explicit | E7, E8 |
| RFC 2 §10 q5 | the block-sized buffer | explicit | §5.1 — open (§14.5) |
| RFC 3 §1.1, §5 | what to box, when to flush, what to evict, read ahead, pre-warm | explicit | §3.1, §4.2, §6.4, §7.1 |
| RFC 3 §4.1, §4.2 | answer the read independently of the fill | explicit | §6.2 |
| RFC 3 §4.5 | decide speculation; make pre-warm yield | explicit | §6.4 — proposal |
| RFC 3 §5 | whether writes continue with the remote unavailable | explicit | §8.2 |
| RFC 3 §5 | aggregate health without latching | caller | §8.1 |
| RFC 3 §8 q3 | how pre-warm yields | explicit | §6.4 — proposal; open (§14.2) |
| RFC 4 §3.4 | order stage, existence, acknowledge | caller | §4.1 |
| RFC 4 §4.3 | return only committed extents | caller | §4.5 |
| RFC 4 §4.4 | serialise commits per file | explicit | §4.3 |
| RFC 4 §6.2 | refuse a commit past a truncation; retry from the journal | caller | §4.4 |
| RFC 4 §6.6 | clone of uncarved content | caller | §9.3 — proposal |
| RFC 4 §8.2 | treat the dedup answer as advisory | caller | §5.3 |
| RFC 4 §13.1 | recognise zero runs | explicit | open (§14.4) |
| RFC 5 §2.5 | supply `Size(file)` at composition | explicit | §2.1 |
| RFC 5 §4.3 | supply release of an inode's refs | caller | §2.1, §9.1 |
| RFC 5 §8.6 | locks never pin bytes | caller | §7.4 |
| RFC 5 §9.2 | flush, evict, fill never advance `mtime` | caller | §4.1 (only the write path writes `mtime`) |
| RFC 5 §9.3 | allocation from the hole set | caller | §2.1, §6.6 |
| RFC 5 §14 q7 | whether the recycle bin is engine policy | explicit | §1.1 — decided: it is not |
| RFC 8 §1.1 | when and what to transfer | explicit | §3.1 |
| RFC 8 §2.5 | syncer obligations live in the engine package | explicit | §1.1, §12.5 |
| RFC 8 §7, §7.1 | each consumer's narrow interface over the backend | caller | §2.1, §2.2 |
| block data flow §3 | orchestration: dedup oracle, manifest rows, scheduling | explicit | §4, §5 |
| block data flow §5 | only the construction site names concrete types | explicit | §2.1 |

The table holds 52 obligations: 27 that name RFC 6 or the engine, and 25 placed
on a caller that can only be the engine. 44 are decided outright. Five are
decided as proposals that name what would overturn them — fill policy, key scope,
speculation and pre-warm's yield (two rows), and clone — and three of those stay
in §14 because their parameters are unmeasured. Three are left open with no
decision: randomised assembly, zero runs, and copies on the flush path.
