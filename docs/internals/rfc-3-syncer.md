---
rfc: 3
title: "RFC 3 — the syncer"
component: syncer
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
aliases:
  - RFC 3
tags:
  - rfc
---
# RFC 3 — the syncer

**Audience:** anyone changing the component that moves chunks between the journal
and the remote tier. Conventions and test tiers are in [the RFC index](rfc-index.md).

---

## In short

- The syncer moves chunks between the journal and the remote tier: out as whole
  blocks, back as the chunks a read needs. It does nothing else.
- It has two halves. The **uploader** puts assembled blocks out, one put each
  (the engine assembles a block from packed chunks,
  [RFC 8 §5.1](rfc-8-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)). The
  **fetcher** gets remote-only chunks back, as ranges of their block or as the
  whole block.
- Each half is a worker pool of configurable size. That pool is the only
  concurrency control, and its size is the memory bound.
- The uploader holds a *reference* into the journal, not a copy. The journal keeps
  those bytes stable until the upload reports.
- The fetcher hands verified chunks to its caller, the engine, which answers the
  waiting read and may store them in the journal (a *fill*).
- Its callers are the engine, one flow per share, and GC, which relocates
  through a flow of its own.
- It names nothing, deletes nothing, decides nothing, and stores nothing.

---

## 1. Purpose

> **Move chunks between the journal and the remote tier, in both directions,
> under a bound: out as whole blocks, back as the chunks that are needed.**

The two directions are deliberately asymmetric. A block is
the unit of **storage**: one name, written by one put, so it goes out
whole. A chunk is the unit of **verification**: each carries its own hash, so any
chunk can be read and checked on its own. A fetch therefore asks for the chunks it
needs — a range of the block — or for the whole block when most of it is wanted
([RFC 8 §6.8](rfc-8-engine.md#6.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block)).

Every transition to **Resident** in this system originates in an outcome this
component reports ([RFC 0 §4.3](rfc-0-data-lifecycle.md#4.3%20Reporting)). Every transition back from **Remote** to holding
local bytes originates in a fetch it performed.

### 1.1 Non-goals

The syncer **MUST NOT**:

- decide *what* to transfer, or *when*, or *why* — that is offload, eviction and
  read-ahead policy ([RFC 0 §5.2](rfc-0-data-lifecycle.md#5.2%20Offload), [§8.1](rfc-0-data-lifecycle.md#8.1%20Evict), [RFC 8](rfc-8-engine.md));
- decide what to delete — that is sweep, which calls the remote tier directly
  ([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep), [RFC 9](rfc-9-gc.md));
- derive a block's name ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block));
- know how a block is encoded, transformed or verified ([RFC 4 §3](rfc-4-remote-tier.md#3.%20The%20block%20format), [§3.3](rfc-4-remote-tier.md#3.3%20Transforms), [§3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec));
- persist anything;
- know what a file, an [extent](rfc-0-data-lifecycle.md#2.1%20Entities) or a [segment](rfc-0-data-lifecycle.md#2.1%20Entities) is, or which files a block's
  [chunks](rfc-0-data-lifecycle.md#2.1%20Entities) came from. To the syncer a chunk is a hash and its bytes, the unit it
  verifies and streams; where chunk boundaries fall, what refers to a chunk, and
  how chunks were grouped into a block are not its concern.

### 1.2 Two halves, one component

| | uploader | fetcher |
| --- | --- | --- |
| Triggered by | a block assembled for offload or relocation | a read of bytes held only remotely |
| Moves | one whole block per transfer | the chunks asked for, as a range of one block, or the whole block |
| Reads from | the journal, by reference | the remote tier |
| Writes to | the remote tier | its caller, which answers the read and may fill the journal |
| Limited by | its own worker pool | its own worker pool |
| Work is | known in advance, and unique | demanded, and may be duplicated |

The halves share a component and share nothing else: no pool, no queue, no
counter. Their sizes **MUST** be configurable independently, because they saturate
on different things — uploads on the uplink and on how fast blocks are assembled,
fetches on latency and on how many readers are blocked.

### 1.3 Interface

Signatures are indicative; the obligations are normative.

```go
type Chunk struct {
    Hash  Hash   // plaintext content hash
    Bytes []byte // borrowed until the next iteration
}

// Range is where one chunk's body sits in an encoded block: a byte offset and a
// length, after transforms, as the block's header records them (RFC 4 §3.2).
type Range struct {
    Off, Len int64
}

// ChunkRange asks for one chunk: the hash to verify it against, and its range.
type ChunkRange struct {
    Hash  Hash
    Range Range
}

// Descriptor is one census entry (RFC 5 §5.3): a transform, its format version
// and the material it used. Opaque to the syncer, which passes it through.
type Descriptor struct {
    Transform uint16
    Version   uint8
    Material  []string
}

// Stored is what a put reports: one Range per chunk, in the order given, and
// the block's census, each distinct Descriptor its bodies used.
type Stored struct {
    Ranges []Range
    Census []Descriptor
}

// Store is what the syncer needs from a backend, declared here and named for
// that need. The engine implements it by composing RFC 4's block codec with a
// remote block store: encoding, transforms and verification happen inside it.
type Store interface {
    // Put encodes the block and streams it to the store. It may call src
    // twice per attempt: once to measure, once to send (RFC 4 §4.3).
    Put(ctx context.Context, name BlockName, src func() iter.Seq2[Chunk, error]) (Stored, error)
    // Get reads the chunks named in want, or the whole block when want is empty.
    // Ranges adjacent in the block are read with one request. Each chunk comes
    // back decoded and verified against its own hash.
    Get(ctx context.Context, name BlockName, want []ChunkRange) iter.Seq2[Chunk, error]
    // Health makes one probe call (RFC 4 §4.7).
    Health(ctx context.Context) error
}

// Every Store error wraps one error of RFC 4's closed set (§4.8) or RFC 5's
// ErrMaterialUnavailable (§2.7), or is a local error returned as itself.

Register(name string, store Store) (StoreID, error)
OpenFlow(store StoreID) (*Flow, error)

func (f *Flow) Upload(ctx context.Context, name BlockName, size int64, src func() iter.Seq2[Chunk, error]) (Stored, error)
func (f *Flow) Fetch(ctx context.Context, name BlockName, want []ChunkRange) iter.Seq2[Chunk, error]
func (f *Flow) Prefetch(ctx context.Context, name BlockName) iter.Seq2[Chunk, error]
func (f *Flow) Healthy() bool
func (f *Flow) Close() error

Close() error // the syncer's own
Stats() Stats // a consistent snapshot of the counters of §2.12
```

Streams are `iter.Seq2`: a caller consumes one with `for c, err := range`, and
leaving the loop early stops the producer.

`Chunk` and `Store` are declared by the syncer, as
[RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy) requires: the syncer depends on neither the carver, the journal nor the
remote tier, and is tested with none of them ([§1.4](#1.4%20It%20is%20testable%20on%20its%20own)). `Chunk` is not the
carver's chunk ([RFC 2 §1.2](rfc-2-carver.md#1.2%20Two%20layers%3A%20the%20chunker%20and%20the%20carver)) under another name. The carver's describes where a
chunk sits in a file; this one is what a transfer carries, a hash and its bytes,
and has no file offset. The engine converts one
into the other. `Hash` is a plain `[32]byte`, so not even the hash type is
shared. `Store` has no delete: the syncer never deletes ([§1.1](#1.1%20Non-goals)).

**The error set is closed.** The syncer uses the remote store's set
([RFC 4 §4.8](rfc-4-remote-tier.md#4.8%20Errors%20are%20a%20closed%20set)) as it is, plus `ErrMaterialUnavailable` from the transforms
([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)); the codec reports every other transform failure as `ErrCorrupt`.
Neither the syncer nor its callers interpret a service's or a transform's own
errors. The error values are all the syncer shares with those components; they
belong to neither, so the syncer still depends on no component's code:

| Error | The syncer | Counts toward health ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) |
| --- | --- | --- |
| `ErrNotFound` | fails the call; the engine re-resolves ([RFC 8 §6.7](rfc-8-engine.md#6.7%20An%20absent%20object%20is%20re-resolved%20exactly%20once)) | no |
| `ErrInvalid` | fails the call, not retried | no |
| `ErrDenied` | fails the call, not retried | yes |
| `ErrTransient` | retries within its bound ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) | yes |
| `ErrCorrupt` | on a put, retries within its bound; on a fetch, fails the call, not retried | no |
| `ErrMaterialUnavailable` | fails the call, not retried; the read fails as the remote being unavailable | no |
| a local error | fails the call | no |

**There is one syncer per process.** An installation may configure several
block stores, and each is registered once. Work reaches the syncer through a
**flow**: a handle bound to one store and to one queue in the fairness scheduler
([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)). A transfer on a flow goes to that flow's store and waits in that flow's
queue, so a transfer cannot be sent to the wrong store, and a call carries
neither a store nor a key. `Flow.Close` takes the flow out of the scheduler; a
call on a closed flow **MUST** fail.

**The syncer does not know what a flow stands for.** It knows stores, flows and
blocks, and nothing about shares, files or tenants. The engine opens one flow
per share, on that share's store, and closes it when the share is removed
([RFC 8](rfc-8-engine.md)). GC opens one flow per store it relocates on, so relocation's reads
and puts get the same memory bound, retries and health refusal as any transfer,
and wait their turn behind no share ([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20name%20by%20content%2C%20put%2C%20then%20move)). GC's deletes do not go through
the syncer ([§1.1](#1.1%20Non-goals)). Fairness per tenant, or a separate flow for pre-warm,
would change which flows are opened and nothing here.

One syncer is what the bounds need: the memory bound is a property of the process
([§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), and a store's health is a property of all the traffic it carries,
from every flow on it ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)). A syncer per share would state neither.

![Three flows, each bound to one store: two queue their transfers per flow, a deficit-round-robin scheduler with a per-flow cap feeds the uploader and fetcher pools, and the pools reach two stores; the third flow's store is unhealthy, its probe keeps running, and its calls are refused before they queue](img/rfc3-overview.svg)

`name` is the block's name ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)). With `want` it is everything
[RFC 4 §3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec)'s verified read needs. The syncer passes both through and **MUST NOT**
interpret them, except to sum the lengths of `want` for scheduling ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)).

**Both directions stream, one chunk at a time.** The header indexes every
chunk's body with its plaintext hash, each body decodes on its own
([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)), and every transform applies per chunk ([RFC 5](rfc-5-transforms.md)). On a put, the
engine's `Store` sends the header, then encodes each chunk as it sends it
([§3.4](#3.4%20One%20put%20per%20block)); on a get, it decodes and verifies each body as it arrives. A worker
therefore holds one chunk and one header, never the whole block, which is what
sets the bound of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound).

**`Upload`** puts the block whose chunks `src` yields from the journal
([§3.2](#3.2%20It%20holds%20a%20reference%2C%20not%20a%20copy)). It hands that stream to the store's `Put`, which encodes and transforms
each chunk beneath the syncer; the syncer never sees a transformed byte
([RFC 4 §3.3](rfc-4-remote-tier.md#3.3%20Transforms)). `size` is the block's largest encoded length, the chain's
`MaxEncodedLen` ([RFC 5 §3.1](rfc-5-transforms.md#3.1%20Interfaces)) summed over its chunks plus the header, which the
engine knows from its plan and the scheduler charges ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)).

It waits in its flow's queue until a worker is free or `ctx` ends, which is the
backpressure of [§2.3](#2.3%20Backpressure%20propagates%3B%20it%20does%20not%20buffer), and fails at once if its store is unhealthy ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) or its
flow's queue is full ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)). It returns the store's `Stored`, and a `nil` error,
only on the store's acknowledgement, which is durable because every store is
([§2.6](#2.6%20Durability%20is%20observed%2C%20never%20inferred)); every other ending, an unknown one included ([§2.5](#2.5%20An%20unknown%20outcome%20is%20not%20a%20success)), is an error.
The ranges and the census go into the block's commit, where later reads and
retirement find them ([RFC 6 §2.2](rfc-6-block-metadata.md#2.2%20Chunk), [RFC 6 §2.3](rfc-6-block-metadata.md#2.3%20Block)).

A retry, and a measuring pass, call `src` again for a fresh stream from the first
chunk, so the bytes behind it **MUST** stay stable until `Upload` returns ([§3.3](#3.3%20The%20journal%20keeps%20referenced%20bytes%20stable)). It **MUST NOT**
call `src` after it returns, which is what lets the caller hand the journal
reference back.

**`Fetch`** is a demand: a reader is waiting. **`Prefetch`** is speculation
([§4.4](#4.4%20Speculation%20does%20not%20delay%20demand)): nobody is waiting yet. They are separate methods, not one method with
a priority parameter, so each call site shows which it means. `Prefetch` always
fetches the whole block: speculation is read-ahead or pre-warm, and both want
whole blocks ([RFC 8 §6.8](rfc-8-engine.md#6.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block)).

**A fetch is a set of whole-chunk ranges of one block.** `want` names the chunks
the caller needs, each by its hash and the `Range` block metadata recorded for it
([RFC 6 §2.2](rfc-6-block-metadata.md#2.2%20Chunk)). An empty `want` asks for the whole block; each body is then
verified against its own hash, and the block's name recomputed from the header
([RFC 4 §3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec)). The store merges ranges adjacent in the block into one ranged get
([RFC 4 §4.4](rfc-4-remote-tier.md#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)), so a packed small file, or a run of one file's chunks, costs one
request ([RFC 8 §5.7](rfc-8-engine.md#5.7%20A%20block%20packs%20chunks%2C%20whichever%20files%20they%20came%20from)); ranges that are not adjacent cost one request each, or
the engine asks for the whole block instead. A range is always a whole chunk: a
range that splits one has no hash to check it against ([§4.1](#4.1%20One%20fetch%2C%20two%20consumers)).

A cold random read asks for only the chunks it covers: fetching a 20 MiB block to
serve one 4 KiB read is read amplification with no correctness benefit. When to
widen a request to the whole block is the engine's policy ([RFC 8 §6.8](rfc-8-engine.md#6.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block)).

Both return a stream of chunks, and:

- **every chunk is verified before it is yielded** ([§4.1](#4.1%20One%20fetch%2C%20two%20consumers)); no byte of an
  unverified chunk reaches the caller;
- **`Bytes` is borrowed until the next iteration**, the same rule as
  the carver's `emit` ([RFC 2 §2.2](rfc-2-carver.md#2.2%20The%20bytes%20handed%20to%20%60emit%60%20are%20borrowed)). A caller that keeps bytes copies them;
- **an error is yielded once and ends the stream; chunks already yielded stay
  valid**, because each was verified on its own;
- **leaving the loop, or cancelling `ctx`, detaches the caller** — from its own
  fetch, or only from a joined one ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)). There is no `Close` to forget.

The syncer's **`Close`** stops accepting work and returns once every transfer in
flight has ended, which [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports) bounds. `Flow.Close` refuses the flow's queued
transfers with an error and lets its running ones finish. A call after either
**MUST** fail. Stores are registered for the life of the process; a store removed
from the configuration is dropped at the next start.

`Store` keeps no health state; `Health` is one probe call ([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)). The syncer
probes each store itself and refuses work for an unhealthy one, and `Flow.Healthy` reports the result
([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)).

### 1.4 It is testable on its own

The syncer's dependencies are exactly four, and each is supplied, never reached
for:

| Dependency | Supplied as | In a test |
| --- | --- | --- |
| a backend | the `Store` interface of [§1.3](#1.3%20Interface) | a fault-injecting fake |
| the bytes to upload | the `src` closure passed to `Upload` | a closure over a byte slice |
| time | the runtime clock | a virtual clock |
| logging | a structured logger | a handler that records the lines |

No journal, no carver, no metadata store, no engine, no disk and no network.
A conformance check **MUST** be writable as: build a syncer over a fake store,
call it, assert on what the fake saw and what came back. An implementation that
picks up another dependency loses that, and **MUST NOT**.

**Time is virtual in tests.** The probe interval, the unhealthy log interval,
retry backoff and every `ctx` deadline run on a virtual clock that advances only
when every task is blocked. A check of the 60-second log line
([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) runs in microseconds and gives the same answer every time. The syncer
therefore **MUST NOT** read time from anywhere the virtual clock cannot replace.
A check that sleeps on the real clock is slow, and on a loaded machine it fails
for reasons that have nothing to do with the syncer.

**The fake store is the fixture every check shares.** Per call it can:

- take a set latency and a set time per byte, so the pool binds ([§7.3](#7.3%20What%20must%20not%20stand%20in));
- fail with any error of the set in [§1.3](#1.3%20Interface), once or every time;
- **commit and then lose the response** — the unknown outcome of [§2.5](#2.5%20An%20unknown%20outcome%20is%20not%20a%20success);
- stop part-way through a put, or corrupt one chunk of a get;
- hold every call until the test releases it;
- fail or pass the probe.

And it records, with the syncer's own per-flow counters beside it: calls in
flight, per store and per flow, and their peak (S1,
S17); the order calls reached it (S13, S18); attempts per block; bytes it holds.

**Memory is checked by holding, then measuring.** With every call held and every
pool full, the heap in use above the idle baseline **MUST** stay within the bound
of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound) plus a stated slack. The test chooses the chunk `Max` and pool size large
enough that the runtime's own noise stays under that slack.

The fake covers the syncer's logic. It does not cover the adapter from the remote
tier to `Store`, nor a real backend's failure modes; those run the same checks
against a local emulator of the remote service ([§7.3](#7.3%20What%20must%20not%20stand%20in)).

## 2. What both halves obey

### 2.1 A worker pool is the only concurrency control

Each half **MUST** be a pool of a configured size, and that size **MUST** be the
only thing limiting how many transfers are in flight. An implementation **MUST
NOT** add a second limiter — a queue depth, a rate limit, a token bucket — whose
interaction with the pool size is unstated.

Two limits that can each be the binding one produce a system whose throughput has
no single explanation, and the one an operator tunes is whichever is not binding.

The per-flow and per-store caps of [§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows) are not second limiters in this sense:
each is a fixed fraction of the pool ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)), so the pool size still determines
it. Nor is a queue's length, which bounds how many transfers wait, not how many
run.

### 2.2 The pool size is a memory bound

Each worker runs one transfer at a time, and a streaming transfer holds only
one chunk, encoded, and the block's header ([§1.3](#1.3%20Interface)). A chunk is at most `Max`
([RFC 2](rfc-2-carver.md), B3) before encoding, and at most the chain's `MaxEncodedLen` of it after
([RFC 5 §3.1](rfc-5-transforms.md#3.1%20Interfaces)), so:

`peak memory of a half = pool size × (MaxEncodedLen(chunk Max) + the header bound)`

Nothing on the path holds the whole block: not the store, whose put takes a
stream ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success)), and not a file on disk. A measuring pass encodes each
chunk and discards it, so it adds CPU, not memory. Every size the syncer bounds,
charges or budgets is an encoded size; the **largest encoded block** is the block
target plus one chunk ([RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component), P2), each chunk at its `MaxEncodedLen`. The whole process's bound is the
uploader's number plus the fetcher's.

The limit belongs to the syncer, not to each store: memory runs out for the whole
process, and only a limit set where all the transfers happen adds up to a number
the process can hold.

### 2.3 Backpressure propagates; it does not buffer

When a pool is full, the caller **MUST** wait or be refused. An implementation
**MUST NOT** accept work into an unbounded queue, and **MUST NOT** introduce a
buffer whose size is not configured.

The chain this preserves runs the whole depth of the system:

remote slows → the pool saturates → offload stalls → dirty [extents](rfc-0-data-lifecycle.md#2.1%20Entities) are not released
→ the journal approaches capacity → writes are refused ([RFC 0 §10.1](rfc-0-data-lifecycle.md#10.1%20Capacity%20is%20a%20bound%2C%20not%20a%20target))

Refusing a write is the specified outcome. An unbounded queue at any link replaces
it with unbounded memory growth, and the process dies instead of declining work.

![The backpressure chain from a slowing remote through to a refused write, and the same chain with one unbounded queue inserted, which ends in the process dying instead](img/rfc3-backpressure-chain.svg)

### 2.4 Every transfer terminates, and reports

Every transfer **MUST** complete in bounded time with either success or a failure
reported to its caller. No transfer retries indefinitely.

The syncer retries only `ErrTransient` ([§1.3](#1.3%20Interface)), within a stated bound; a
throttling response is transient. Misclassification either way is recoverable,
because both paths end in a report.

**Bounded time is enforced by throughput, not by a fixed deadline.** A fixed
per-transfer timeout is too short for a maximum-size block on a slow link and far
too long for a connection that has stopped moving. A transfer **MUST** instead
fail when its throughput stays below a stated floor for a stated interval. The
floor is fixed ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) at 64 KiB/s over 30 s: below any working link, and high
enough that a maximum-size block cannot crawl for more than about five minutes. A
floor of a few bytes per second catches a stalled connection but not a crawling
one. Time to the first byte is bounded separately, since a request can stall
before any byte moves.

Only the store's side counts. Time a caller spends not reading a fetched stream is
the caller's, and never trips the floor: a consumer that stops reading is
[§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)'s to handle, by detaching it, not a reason to fail the transfer for every
caller joined to it.

**A slow tail is retried, not waited out.** A transfer whose first byte is
later than a bound derived from recent latency, such as the 90th percentile,
**SHOULD** be cancelled and retried on a new connection, within the retry bound.
Sending a second request alongside the first and taking whichever answers
(hedging; Dean and Barroso, *The Tail at Scale*, 2013) cuts tail latency further
at a few percent more requests; it is deferred until a measurement of cold-read
latency asks for it.

**The syncer is the only layer that retries** ([RFC 4 §4.9](rfc-4-remote-tier.md#4.9%20No%20state%20across%20calls)). A streamed body cannot
be rewound by a client that has already sent part of it; only the syncer, which
can call `src` again ([§1.3](#1.3%20Interface)), can retry one.

### 2.5 An unknown outcome is not a success

A transfer can end with its outcome genuinely unknown: the request was sent, the
response was lost. The syncer **MUST** resolve that as *not durable* and report it
as a failure.

It happens on every network backend. A put is sent and the service stores the
block, then the connection resets, or the client's timeout fires, before the
response arrives. The block exists and the syncer cannot know it. Equally, the request
may have died on the way out and nothing exists. Both look the same from the
client.

This is safe only because a block's name is a function of its content ([RFC 2](rfc-2-carver.md)
[§4.2](rfc-2-carver.md#4.2%20A%20block)), which makes the retry idempotent in content
([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success)): the name covers the chain as well as the chunks, and encoding is
deterministic, so the retry writes the very bytes the lost put may have stored.

The idempotence covers a retry of the same block: `Upload` retrying, or the engine
re-offering the same plan. A later pass that groups the chunks differently derives
a different name, and a block the earlier attempt may have written is then an
orphan until garbage collection finds it ([RFC 9](rfc-9-gc.md)). The engine keeps the plan of
any block whose outcome was unknown, in memory, and offers it again, unchanged,
before packing anything new ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)), so that is the exception rather than the
routine; a restart is when it happens.

![A put whose response was lost leaves three indistinguishable remote states; a retry under the same content-derived name converges all three to one object](img/rfc3-unknown-outcome.svg)

![Three orders of naming and recording a block, each crashed at its worst moment: a key recorded before the put leaves a dangling reference, a put before the record leaves an orphan only a listing can find, and a key derived from the content makes the retry write the same object](img/rfc3-key-derivation.svg)

### 2.6 Durability is observed, never inferred

The syncer **MUST** report durability only on the acknowledgement [RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success)
defines for the backend in use. None of the following is evidence, and an
implementation **MUST NOT** report durability on any of them:

| Not evidence | Why it looks like evidence |
| --- | --- |
| the call returned | it returned from a buffer, not from storage |
| no error was observed | an error that is never delivered is not an absence of error |
| the bytes were all sent | sent is not stored |
| enough time has passed | time is not an acknowledgement |
| a later read succeeded | the read may be served from a cache the write populated |

Every store is durable, and what counts as its acknowledgement is fixed per
backend by [RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success); there is no setting that declares a store non-durable.

### 2.7 It reports; it does not persist

The syncer **MUST NOT** persist durability, residency or health. It returns what
it observed; [RFC 6](rfc-6-block-metadata.md) records it and [RFC 1](rfc-1-journal.md) is told ([RFC 0 §4.3](rfc-0-data-lifecycle.md#4.3%20Reporting)).
A durable record of its own would be a third record beside block metadata and
the journal, and nothing resolves a three-way disagreement after a crash.

### 2.8 An unhealthy store refuses work

A store's health belongs to the store. Every backend exposes a liveness probe —
one round trip, no state ([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)) — and the syncer calls it to decide
whether the store is **healthy** or **unhealthy**:

- the syncer **MUST** probe every registered store at a fixed interval, and
  **SHOULD** probe at once when a transfer to it fails with `ErrTransient` or
  `ErrDenied`;
- a failed probe makes the store unhealthy; a successful one makes it healthy;
- a store also turns unhealthy when every transfer to it over a stated window
  ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) failed with `ErrTransient` or `ErrDenied`, although its probe passed —
  a store that answers the probe but throttles puts, or refuses them after a
  permission change. Only those two errors count: `ErrNotFound`, `ErrCorrupt`
  and `ErrMaterialUnavailable` say something about one block or its material, and
  a local error about this host, not about the store ([§1.3](#1.3%20Interface)). The store turns
  healthy on the next successful probe, so such a store is retried once per probe
  interval rather than continuously, and never latched;
- an unhealthy store **MUST** keep being probed. Once transfers stop, the probe
  is the only thing that can observe recovery, so without it unhealthy would be
  a latch (below);
- a probe **MUST** be bounded in time like any call, and one that does not
  return within its bound is a failed probe. A hung probe that left the store
  healthy would keep sending traffic to a dead store.

**An unhealthy store refuses work.** `Upload`, `Fetch` and `Prefetch` targeting it
**MUST** fail at once, before taking a worker and without calling the backend.
Retrying a request the syncer already knows will fail spends a worker, a round
trip and a timeout to learn nothing, and a pool full of such retries stalls every
flow on every other store. The refusal is cheap and certain; the probe, not the
traffic, is what finds out when the store is back.

**Health is probed, never stored as a flag.** Neither a store's health nor a
share's ([RFC 8 §8.1](rfc-8-engine.md#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included)) **MUST** be a "remote is down" flag that stops attempts until
something clears it. Once attempts stop, none can succeed, so nothing ever sees
the remote come back, and a five-minute outage becomes permanent.

![A latched flag suppresses the attempts that would observe the recovery, so it is never cleared; a probe that runs whether or not transfers do observes the recovery, and one success makes the store healthy again](img/rfc3-health-latch.svg)

A transfer already running when the store turns unhealthy is left alone. It
succeeds or fails on its own, within [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)'s bound; nothing cancels a put
mid-flight.

`Flow.Healthy` reports the current state of the flow's store. It reads memory and never calls the
backend, so a caller can check it before doing work that would end in a refused
upload — the engine skips carving and packing an offload for a store it would
refuse ([RFC 8 §8.1](rfc-8-engine.md#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included)). The state is re-derived from the next probe, so it is not
persisted ([§2.7](#2.7%20It%20reports%3B%20it%20does%20not%20persist)).

Only the flows on that store are affected — with one flow per share, only that
store's shares. For them:

- a read that needs the store fails at once;
- a write still lands in the journal, and is refused once the journal fills
  ([RFC 0 §10.1](rfc-0-data-lifecycle.md#10.1%20Capacity%20is%20a%20bound%2C%20not%20a%20target)), so an outage shorter than the journal's headroom costs writers
  nothing.

Every other flow carries on.

#### What an unhealthy store logs

It **MUST** be obvious from the logs, and logging it **MUST NOT** cost in
proportion to the traffic it refuses. So the syncer logs the store, not the calls:

| When | Level | One line carrying |
| --- | --- | --- |
| the store turns unhealthy | error | the store's name, the probe's error, the last transfer error |
| every configured interval while unhealthy | warn | the store's name, how long it has been unhealthy, the latest probe error, how many uploads and fetches were refused since the last line |
| the store turns healthy | info | the store's name, how long it was unhealthy, how many calls were refused in total |

Nothing else about it is logged at info or above: not each failed probe, not each
refused call. Counts are counters, incremented without formatting anything, and
read only when a line is written. The volume is two lines per outage plus one per
interval, whatever the load.

A refused call returns an error that names the store and wraps the probe's
error, so the caller can tell an unhealthy store from a failed transfer without
logging its own diagnosis. A caller **SHOULD** log such an error at debug at
most: the syncer's lines are the record of the outage.

### 2.9 Workers are shared fairly across flows

One pool serves every flow ([§1.3](#1.3%20Interface)), so the order in which waiting transfers
get a worker decides whether one flow can starve another. Arrival order cannot
be used: a flow with a backlog of a million uploads would put every other
flow's read behind all of them. Neither can a pool per flow, whose total grows
with the number of flows and breaks [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound).

The design is taken rather than invented: **Deficit Round Robin** (DRR;
Shreedhar and Varghese, SIGCOMM 1995) for turns, with per-flow queues, a
borrowing cap and refusal of a full queue as in API-server priority-and-fairness
admission control, which solves the same problem: one shared concurrency pool,
many flows, none allowed to starve another. A flow is whatever the caller wants
kept apart.

Each half runs its own scheduler over its own pool:

- **One queue per flow.** A transfer waits in its flow's queue, never in a
  global one.
- **Turns are in bytes, not requests.** When a worker frees, the scheduler visits
  the next flow with work waiting, adds a **quantum** of bytes to that flow's
  **deficit**, and dispatches from the head of its queue while the head's size
  fits within the deficit, subtracting each one's size. A flow whose queue
  empties has its deficit reset to zero. A transfer is charged its encoded size:
  an upload the `size` it declares, a fetch the sum of its `want` ranges' lengths,
  and a whole-block fetch the largest encoded block ([§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), since its size is
  unknown. A flow sending large blocks therefore gets no more bandwidth than one
  sending small ones.
- **The quantum is at least the largest encoded block**, so every flow with work
  waiting dispatches at least once per round. Each dispatch costs O(1).
- **Two levels: stores, then flows.** The scheduler first picks a store, by the
  same round robin over stores with work waiting, then a flow on that store.
  Without the store level, eight shares on one slow store hold eight flows' caps
  between them — the whole pool — and the flows on a healthy store starve.
- **Within a flow, demand goes before speculation** ([§4.4](#4.4%20Speculation%20does%20not%20delay%20demand)). The scheduler
  picks the flow; the flow's queue picks the transfer.
- **No store, and no flow, holds more than a fixed cap of the pool's workers at once**,
  stated as a fraction of the pool. Fair turns only
  decide who gets the *next* free worker: without the cap, a flow or a store that is
  slow but healthy can hold every worker for as long as its puts take, and no
  turn comes round. The cap is not a reservation. A flow alone on the system uses
  up to the cap, and no worker sits idle waiting for a flow that has no work.
- **Each queue has a fixed length.** A call that finds its flow's queue full
  **MUST** be refused. Waiting outside the queue would be
  an unbounded queue under another name ([§2.3](#2.3%20Backpressure%20propagates%3B%20it%20does%20not%20buffer)). The refusal travels back to
  whoever opened the flow — for a share's flow, that share's journal — and
  no other flow's work is refused because of it.
- **A waiting transfer leaves its queue when its `ctx` ends**, and a transfer to
  an unhealthy store never enters one ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)).

A *round* is one visit to every flow with work waiting. A transfer at the head of
its flow's queue is therefore dispatched within one round — at most one turn for
each other flow with work waiting — unless its own flow is at its cap.

![One DRR round with a 20 MiB quantum: a flow of 16 MiB blocks sends one and carries 4 MiB over, a flow of 4 MiB blocks sends five, a flow with one 20 MiB block sends it; below, the cap skipping a flow that already holds six of eight workers on a slow store](img/rfc3-drr-round.svg)

All flows have equal weight. Weighting one flow over another is DRR's
per-flow quantum and needs no other change; it is left out until an operator
needs it.

### 2.10 Two settings, and everything else fixed

The syncer exposes exactly two settings, both process-wide:

| Setting | Sizes | Default |
| --- | --- | --- |
| `upload_workers` | the uploader's pool | 32 |
| `fetch_workers` | the fetcher's pool | 32 |

Neither default follows the CPU count. A transfer spends its time waiting on the
network, so the count that keeps a link busy follows latency and bandwidth: by
Little's law, workers ≈ throughput × time per transfer ÷ block size. At a 200 ms
transfer of a 16 MiB block, 32 workers sustain about 2.5 GiB/s, beyond most links.
Deriving it from cores would also scale memory with cores, and give a small VM
on a fast link too few.

Each is a memory budget as much as a concurrency limit ([§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), and the
documentation of each **MUST** state the memory it implies at the configured
chunk `Max` and chain. Both are fixed for the life of the process ([§2.11](#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)).

Everything else the syncer needs is derived or fixed, and **MUST NOT** be a
setting:

| Value | Is | Because |
| --- | --- | --- |
| DRR quantum | the largest encoded block ([§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound)) | [§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows) needs it to be at least that, and nothing is gained above it |
| per-store and per-flow cap | three quarters of the pool, rounded down, at least one worker; with a pool of more than one, at most the pool less one | leaves a quarter for every other flow while letting a flow alone use most of the pool |
| per-flow queue length | four times the pool | a waiting transfer holds a reference, not bytes ([§3.2](#3.2%20It%20holds%20a%20reference%2C%20not%20a%20copy)), so the length bounds bookkeeping, not memory |
| probe interval | 5 s, healthy or not | one interval; one failed probe is unhealthy, one success healthy ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) |
| failure window | 30 s | a store whose every transfer failed for this long, with its probe passing, turns unhealthy ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) |
| throughput floor | 64 KiB/s over 30 s, and 10 s to the first byte | [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports) |
| detach bound | 5 s without taking the next chunk | a joined caller that falls behind is detached ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) |
| unhealthy log interval | 60 s | [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)'s summary line |
| retry bound | the syncer's own, stated by the implementation | [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports) |

Each fixed value becomes a setting only when a measurement shows the fixed value
is wrong for a workload an operator can name. A setting nobody can reason about
is tuned by trial, and a wrong value shows up as a stall with no stated cause.

Read-ahead depth, pre-warm pacing and when to offload are policy and belong to
[RFC 8](rfc-8-engine.md), not here. Chunk and block sizes belong to [RFC 2](rfc-2-carver.md).

### 2.11 Pool sizes are measured once, by a tool

The pools do not adapt at runtime. The documented way to size them is a separate
tool that measures a store and prints the two settings; the operator writes them
into the configuration, and they hold for the life of the process ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)).
A number printed once can be checked against a real bucket. A controller that
resizes the pool while serving can only be judged by watching it, and its worst
failure — a pool that shrinks during a slowdown — looks like a slow network from
outside.

The tool measures pure transfer: puts and gets of blocks the size of the largest
encoded block, at rising concurrency, against its own instance of the store under
a scratch prefix no share uses ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)), deleting what it wrote. It prints
the pool sizes and the memory each implies ([§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound)). Its result describes this
host, this link and this time of day, and nothing re-runs it automatically. How
it runs is the tool's own documentation, not this document's. It does not see
the transforms' CPU cost; where the chain cannot keep up with the printed pool,
the pool is larger than the process can use, and only memory is wasted.

### 2.12 What the syncer makes observable

The syncer keeps counters, as [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)'s log lines already require, and returns a
consistent snapshot of them from a `Stats` call on the syncer. It depends on no
metrics library ([§1.4](#1.4%20It%20is%20testable%20on%20its%20own)); the engine exports the snapshot. Taking a snapshot **MUST NOT** take a lock a dispatch
needs, and its cost **MUST NOT** grow with the number of transfers in flight.

Every metric is labelled with `half` (`upload` or `fetch`) and `store`. Queue
metrics are also reported per flow; the syncer knows a flow by its id, and the
engine replaces it with the share when it exports ([§1.3](#1.3%20Interface)). The syncer also
reports each queue metric summed over all flows, so the total needs no query
across series and survives the engine capping how many shares it exports one by
one. The syncer
**MUST** make every metric below observable:

**Pools** — is the syncer the limit?

| Metric | Type | Answers |
| --- | --- | --- |
| `dittofs_syncer_workers` | gauge | the pool size ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) |
| `dittofs_syncer_workers_busy` | gauge | workers transferring. A full pool while throughput is low means the store is slow; a pool that is not full while writes back up means the limit is upstream ([§7.4](#7.4%20Benchmarks)) |
| `dittofs_syncer_inflight_bytes` | gauge | bytes held by transfers in flight, against the bound of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound) |
| `dittofs_syncer_queue_depth` | gauge | transfers waiting, per flow and in total ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)) |
| `dittofs_syncer_queue_wait_seconds` | histogram | time from call to dispatch; a flow whose wait exceeds one round is being starved (S18) |
| `dittofs_syncer_refused_total` | counter | calls refused, labelled `reason` = `unhealthy`, `queue_full` or `closed` |

**Transfers** — what moved, and how it ended?

| Metric | Type | Answers |
| --- | --- | --- |
| `dittofs_syncer_transfers_total` | counter | transfers ended, labelled `outcome` = `ok`, `failed` or `unknown` ([§2.5](#2.5%20An%20unknown%20outcome%20is%20not%20a%20success)) |
| `dittofs_syncer_bytes_total` | counter | encoded bytes transferred |
| `dittofs_syncer_transfer_duration_seconds` | histogram | time from dispatch to end |
| `dittofs_syncer_first_byte_seconds` | histogram | time to the first byte; the tail that slow-tail retries and hedging aim at ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) |
| `dittofs_syncer_retries_total` | counter | attempts beyond the first, labelled `reason` = `transient`, `slow_tail` or `floor` ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) |

**Fetches** — are reads being shared?

| Metric | Type | Answers |
| --- | --- | --- |
| `dittofs_syncer_fetch_joined_total` | counter | callers that joined a fetch already in flight ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) |
| `dittofs_syncer_fetch_detached_total` | counter | joined callers detached for falling behind |
| `dittofs_syncer_speculation_preempted_total` | counter | speculative fetches that yielded to demand ([§4.4](#4.4%20Speculation%20does%20not%20delay%20demand)) |

**Health** — is each store usable?

| Metric | Type | Answers |
| --- | --- | --- |
| `dittofs_syncer_store_healthy` | gauge (0/1) | the latest probe's verdict ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) |
| `dittofs_syncer_store_unhealthy_seconds_total` | counter | time spent unhealthy |
| `dittofs_syncer_store_transitions_total` | counter | changes of state; a high rate is flapping ([§9](#9.%20Open%20questions), question 2) |
| `dittofs_syncer_probe_duration_seconds` | histogram | time per probe |

`workers_busy` against throughput tells a slow store from a starved syncer,
which the logs cannot ([§7.4](#7.4%20Benchmarks)).

Verification is not counted here. Each event has one owner: the block codec
counts chunks verified and chunks that failed ([RFC 4 §4.12](rfc-4-remote-tier.md#4.12%20What%20a%20store%20makes%20observable)); the remote store
counts requests, so gets against chunks fetched shows how well adjacent ranges
merge; transforms count their own decode failures ([RFC 5 §6](rfc-5-transforms.md#6.%20Observability)).

## 3. The uploader


### 3.1 It is triggered, not scheduled

A block becomes the uploader's work when the engine has assembled it, for
offload or, on GC's flow, for relocation. The uploader **MUST NOT** decide which
blocks to assemble, when, or in what order to take them.

### 3.2 It holds a reference, not a copy

The uploader **MUST** take a reference to the block's bytes in the journal and
read them, chunk by chunk, while a worker transfers the block. It **MUST NOT**
copy the block into memory when the block is queued.

The reference is to the bytes **as offered for offload**, not to the file as it is
now. A client may overwrite an extent while its block waits for a worker
([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload)); a reference by file offset would then read the newer bytes, whose
hash is not the block's. So the engine describes a block as a plan — its name and,
per chunk, a hash and where its bytes sit in the offered version — and `src` reads
the plan through the journal's `offered` readers, which keeps returning the offered
bytes until the offload callback returns ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)). A block may hold chunks of
several files ([RFC 8 §5.7](rfc-8-engine.md#5.7%20A%20block%20packs%20chunks%2C%20whichever%20files%20they%20came%20from)), so one plan may read through several files'
readers. The syncer sees none of this: to it, `src` is a stream of chunks, and a
block is the same thing whether its chunks came from one file or from a hundred.

The difference is the bound of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound). A copy taken at queue time is held for the
whole time the block waits for a worker, so peak memory follows the queue depth;
a reference is materialised only while a worker is transferring, so it follows the
pool size.

![A block waiting for a worker holds only a plan of chunk hashes and offsets into the offered records; the worker walks the plan through the offered reader, re-hashing each chunk, and streams it to the store's put; a newer record written meanwhile is never read](img/rfc3-upload-plan.svg)

### 3.3 The journal keeps referenced bytes stable

The journal keeps offered bytes stable until the offload callback that offered them
returns ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload)): it **MUST NOT** release, overwrite, relocate or compact them
before then. The engine runs the upload inside that callback
([RFC 8 §5.1](rfc-8-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)), so no reference outlives it and `src` is never called after
`Upload` returns.
This is the other half of [§3.2](#3.2%20It%20holds%20a%20reference%2C%20not%20a%20copy), and a requirement on [RFC 1](rfc-1-journal.md), not on this component.

### 3.4 One put per block

A block is transferred by a single put of the whole block ([RFC 4 §4.1](rfc-4-remote-tier.md#4.1%20Interface)). A put that
does not complete **MUST NOT** leave the block retrievable and **MUST NOT** be
reported durable.

Where a backend's client splits a put into parts beneath the syncer, durability
is the completion of the whole and **MUST NOT** be inferred from the parts.

The uploader **MUST NOT** use multipart uploads. A block is tens of MiB at most
([RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component)), and the pool already fills the link across blocks, so multipart would
only add a capability every backend must advertise and abandoned parts that are
billed and invisible to a listing. Revisit if a measurement shows one put of a
maximum-size block cannot saturate the uplink even with the pool full.

**The put streams.** The store needs the block's length, and on some services a
checksum, before the first byte ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success)). The engine's `Store` takes them
from each transform's declared length when the chain allows it, and otherwise
from a measuring pass that encodes each chunk, keeps its length and checksum, and
discards it; then it sends the header and encodes each chunk again as it sends
it. Encoding is deterministic, so both passes produce the same bytes
([RFC 5 §2.1](rfc-5-transforms.md#2.1%20A%20transform%20acts%20on%20one%20chunk)). The measuring pass costs a second encode and a second read of
the offered bytes, usually from page cache; it costs no disk and no memory beyond
[§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound).

### 3.5 The bytes are stable for the duration

The bytes a put transfers **MUST NOT** change while it runs. [§3.3](#3.3%20The%20journal%20keeps%20referenced%20bytes%20stable) supplies that
for a journal reference; an implementation that transfers from anywhere else
**MUST** supply it some other way. As a second line, `src` recomputes each
chunk's hash as it reads and fails the transfer on a mismatch
([RFC 8 §5.1](rfc-8-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)): a violation becomes a refused put, not a misnamed block.

## 4. The fetcher

### 4.1 One fetch, two consumers

A fetch of chunks held only remotely — some ranges of one block, or all of it —
produces verified chunks, one at a time, and yields them to its caller. The caller
is the engine, which uses them twice: to answer the read that caused the fetch,
and, where its fill policy says so, to fill the journal so the next read is local
([RFC 8 §6.2](rfc-8-engine.md#6.2%20The%20reply%20is%20served%20from%20the%20fetched%20bytes), [RFC 0 §2.3](rfc-0-data-lifecycle.md#2.3%20Operations)). The syncer imports no journal and fills nothing ([§1.4](#1.4%20It%20is%20testable%20on%20its%20own)).

Both uses **MUST** read the same bytes and **MUST NOT** modify them. Each chunk is
verified before it is yielded, which is what makes one buffer safe for both. A
reader waiting on one chunk is answered when that chunk arrives, not when the
whole range has.

![A fetch is verifiable exactly when the caller can name a hash for what it returns: a chunk-aligned range is checked against the chunk's hash, an arbitrary byte range has no hash anywhere and would be returned on trust](img/rfc3-partial-retrieval.svg)

### 4.2 The reply neither waits on the fill nor fails with it

That the reply neither waits on the fill nor fails with it is the engine's rule
([RFC 8 §6.2](rfc-8-engine.md#6.2%20The%20reply%20is%20served%20from%20the%20fetched%20bytes)). The syncer's part is to yield each chunk as soon as it is verified,
valid until the caller's next iteration ([§1.3](#1.3%20Interface)).

### 4.3 Concurrent demand for one chunk is one fetch

When a fetch that will bring a chunk is already in flight, a second demand for
that chunk **MUST** join it rather than start another.

Without this, N readers arriving together on one cold chunk cost N transfers, N
times the per-transfer memory of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound), and N fills of identical bytes. The uploader needs
no equivalent rule, because an assembled block is work that exists once.

Joining requires each of the following:

- **The fetch is keyed by store, block and chunk.** Two readers of one chunk join,
  wherever in the chunk each read starts. A whole-block fetch joins only another
  whole-block fetch of the same block; a chunk fetch may join a whole-block fetch
  that has not yet passed that chunk. The chunk, not the byte offset, is the unit
  of joining, and the store is in the key because one content-derived name can
  exist on two stores.
- **A joined fetch belongs to the flow that started it.** A caller that joins takes
  no worker and no turn of its own; the fetch keeps its starter's place in the
  scheduler and counts against its starter's caps.
- **One caller leaving does not cancel it.** A joined fetch runs while any caller
  still waits on it. The first reader's timeout **MUST NOT** fail the others.
- **A failure reaches every caller and is not kept.** Each joined caller gets the
  error; the next demand after it starts a fresh fetch. A remembered failure is a
  latch ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)).
- **A demand joining a speculative fetch promotes it.** From then on it is a
  demand and [§4.4](#4.4%20Speculation%20does%20not%20delay%20demand) applies to it as one; a reader **MUST NOT** wait at
  speculative priority because a guess got there first.
- **No caller holds the others back for long.** A chunk is released once every
  attached caller has moved past it, and nothing is buffered for a caller that is
  behind ([§2.3](#2.3%20Backpressure%20propagates%3B%20it%20does%20not%20buffer)). A caller that has not taken the next chunk within the detach
  bound ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) is detached: its stream ends with an error saying so, and it
  fetches again or reads what a fill left. A speculative caller is detached as
  soon as it is the only one holding a demanded caller back. A stalled reader
  therefore costs a worker and a chunk buffer for seconds, not for as long as its
  client takes.
- **A late caller gets what is still to come.** It receives the chunks not yet
  delivered when it joined. If the chunk it needs has already gone past, it
  starts a new fetch rather than waiting for one that will never bring it.

![One block of six chunks fetched once: the first reader leaves after four chunks and the fetch goes on, a second reader joins after two and receives the remaining four, a third needs the first chunk after it has gone past and starts a new fetch](img/rfc3-joined-fetch.svg)

### 4.4 Speculation does not delay demand

A fetch is **demanded** when a reader is blocked on it and **speculative**
otherwise: read-ahead or pre-warm ([§4.5](#4.5%20Speculation%20is%20executed%20here%20and%20decided%20elsewhere), [RFC 0 §2.3](rfc-0-data-lifecycle.md#2.3%20Operations)). A speculative fetch **MUST NOT** delay a demanded one, and **MUST** be
abandonable without failing any read.

A single queue serving both, first come first served, makes a client wait behind a
guess. Sharing one pool is permitted; sharing one arrival order is not.

### 4.5 Speculation is executed here and decided elsewhere

The fetcher executes fetches. Which blocks are worth fetching before they are
asked for is policy, and belongs with the component that sees the access pattern
and the free capacity ([RFC 8](rfc-8-engine.md)). Two kinds exist and they differ in more than scale:

| | read-ahead | pre-warm |
| --- | --- | --- |
| Triggered by | an observed access pattern | an explicit request |
| Size | the next few blocks | up to a whole subtree |
| A reader is waiting | soon, probably | no |
| Abandoning it costs | a later demand fetch | the request, which can be reissued |

So the syncer's speculation surface is mechanism only: `Prefetch`, and
cancellation through its context ([§1.3](#1.3%20Interface)). It takes no hint about what to
fetch next and keeps no access history.

Both are speculative under [§4.4](#4.4%20Speculation%20does%20not%20delay%20demand). Pre-warm carries one further obligation — it
must not fill the journal until writes are refused ([RFC 0 §10.1](rfc-0-data-lifecycle.md#10.1%20Capacity%20is%20a%20bound%2C%20not%20a%20target)) — and that
obligation is the engine's ([RFC 8 §6.4](rfc-8-engine.md#6.4%20Speculation%20is%20planned%20here%2C%20and%20yields%20to%20demand%20and%20to%20writes)), because only the engine sees journal capacity.
The syncer's part is that a `Prefetch` can be abandoned at any point without
failing anything. An engine that runs pre-warm on a flow of its own ([§1.3](#1.3%20Interface)) also
keeps it from holding its share's flow at its cap while that share's reads wait.

## 5. What belongs elsewhere

| Concern | Owner |
| --- | --- |
| A block's name | [RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block) |
| Encoding, transforms, verification, the remote store's errors | [RFC 4 §3](rfc-4-remote-tier.md#3.%20The%20block%20format), [§3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec), [§4.8](rfc-4-remote-tier.md#4.8%20Errors%20are%20a%20closed%20set), [RFC 5](rfc-5-transforms.md) |
| What a durable acknowledgement is, per backend | [RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success) |
| Recording durability and residency | [RFC 6](rfc-6-block-metadata.md) |
| Keeping referenced bytes stable; filling fetched ones | [RFC 1](rfc-1-journal.md) |
| Deleting a remote block; what to relocate | [RFC 9](rfc-9-gc.md), deleting through [RFC 4](rfc-4-remote-tier.md) directly |
| What to assemble, when to offload, what to evict, what to read ahead or pre-warm | [RFC 8](rfc-8-engine.md) |
| Whether the system keeps accepting writes when the remote is unavailable | RFC 8 |
| A share's health, from its store's state and its own offload outcomes | [RFC 8 §8.1](rfc-8-engine.md#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included) |

## 6. Invariants

| # | Invariant |
| --- | --- |
| S1 | In-flight transfers per half never exceed that half's configured pool size. |
| S2 | Peak memory per half is at most the pool size times the per-transfer figure [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound) states. |
| S3 | Backpressure reaches the caller; no unbounded queue exists. |
| S4 | Every transfer ends in a success or a reported failure, in bounded time. |
| S5 | An unknown outcome is reported as not durable. |
| S6 | Durability is reported only on an acknowledgement that means durable. |
| S7 | The syncer persists nothing. |
| S8 | Bytes referenced by an in-flight upload do not change or move. |
| S9 | A partially transferred block is never retrievable and never reported durable. |
| S10 | Every fetched chunk is verified before any consumer sees it. |
| S11 | A chunk is yielded as soon as it is verified, and stays valid until its caller's next iteration. |
| S12 | Concurrent demand for one chunk produces one fetch of it. |
| S13 | A demanded fetch is never delayed by a speculative one. |
| S14 | A call to an unhealthy store fails without taking a worker or calling the backend. |
| S15 | An unhealthy store is probed until it is healthy. |
| S16 | Log volume while a store is unhealthy does not grow with traffic. |
| S17 | No store and no flow holds more workers of a half than its cap. |
| S18 | A transfer at the head of its flow's queue is dispatched within one round of the other waiting flows. |
| S19 | Every `Store` error is one of the closed set, and only `ErrTransient` and `ErrDenied` count toward a store's health. |

## 7. Conformance

The set-wide rules are in [the RFC index](rfc-index.md#Test%20tiers). The checks below are for the
requirements that fail *silently*. Every check in [§7.1](#7.1%20Group%20A%20%E2%80%94%20silent%20data%20loss) requires a backend that can lose, corrupt, delay and
half-complete. A backend that always succeeds asserts nothing about any of them.

### 7.1 Group A — silent data loss

| Requirement | Check |
| --- | --- |
| [§2.5](#2.5%20An%20unknown%20outcome%20is%20not%20a%20success) unknown is failure | Drop the response of a put the backend committed; assert failure is reported, then that the retry produces one block, not two. |
| [§2.6](#2.6%20Durability%20is%20observed%2C%20never%20inferred) no inference | Drive the real backend against a service that closes the connection after receiving the whole body and before responding; assert `Upload` returns an error. |
| [§3.4](#3.4%20One%20put%20per%20block) partial put | Interrupt a transfer; assert the name is not retrievable and was not reported durable. |
| [§3.3](#3.3%20The%20journal%20keeps%20referenced%20bytes%20stable), [§3.5](#3.5%20The%20bytes%20are%20stable%20for%20the%20duration) stability | Give `Upload` a `src` whose bytes change mid-stream; assert the put is refused and nothing is stored under the name. That the journal keeps offered bytes stable is [RFC 1](rfc-1-journal.md)'s check. |
| [§4.1](#4.1%20One%20fetch%2C%20two%20consumers) verification | Corrupt the fetched bytes; assert no byte of the chunk is yielded and the error is `ErrCorrupt`. |
| [§3.4](#3.4%20One%20put%20per%20block) measuring pass | Upload through a store that measures first; assert `src` is read twice per attempt, the bytes sent equal the bytes measured, and a `src` that changes between the passes fails the put. |
| [§1.3](#1.3%20Interface) census | Put a block whose chunks use two transforms and two materials; assert `Upload` returns every distinct descriptor, and none on a failed put. |
| [§4.2](#4.2%20The%20reply%20neither%20waits%20on%20the%20fill%20nor%20fails%20with%20it) yield at once | Hold one caller mid-iteration; assert the chunk it holds is unchanged until its next iteration, and that each chunk is yielded before the next is read from the store. |

### 7.2 Group B — wedging and unbounded resource use

| Requirement | Check |
| --- | --- |
| [§2.1](#2.1%20A%20worker%20pool%20is%20the%20only%20concurrency%20control) one limiter | Saturate each half; assert in-flight never exceeds its pool size and that no other limit binds first. |
| [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound) memory bound | Stall the backend at full pools; assert peak bytes stay within pool size times the stated per-transfer figure, per half and summed. |
| [§2.3](#2.3%20Backpressure%20propagates%3B%20it%20does%20not%20buffer) backpressure | Stall the backend and keep submitting; assert callers block or are refused rather than queue. |
| [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports) termination | Present a permanently unavailable backend; assert every transfer returns within its bound. |
| [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work) refusal contained | Fail one store's probe under load; assert calls to it fail at once without a backend call, and transfers to a second store still start. |
| [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work) recovery | Fail a store's probe, then restore the backend; assert it turns healthy and accepts calls without any other action. |
| [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work) logging | Keep a store unhealthy under heavy load for several intervals; assert exactly one unhealthy line, one healthy line and one line per interval at info or above, and that a refused call's error names the store. |
| [§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows) no starvation | Saturate flow A with uploads to a store whose puts take seconds; issue one fetch on flow B; assert it starts within one round and flow A never holds more than its cap. |
| [§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows) fair by bytes | Run two flows, one uploading maximum-size blocks and one small blocks; assert bytes transferred per flow stay within one quantum of each other. |
| [§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows) bounded queue | Fill flow A's queue; assert A's next call is refused while flow B's calls are still accepted. |
| [§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch) single flight | Demand one cold chunk from N readers at once, at different offsets within it; assert one `Get`. |
| [§4.4](#4.4%20Speculation%20does%20not%20delay%20demand) priority | Saturate the fetch pool with speculation, then demand a block; assert the demand is not queued behind it. |
| [§4.4](#4.4%20Speculation%20does%20not%20delay%20demand) abandonable | Cancel a `Prefetch` mid-stream while a demand has joined it; assert the demand completes and nothing fails. |
| [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work) failure window | Pass the probe while failing every put with `ErrTransient` for the window, then again with `ErrDenied`; assert the store turns unhealthy, then healthy on the next probe, and that puts are retried once per probe interval, not continuously. Fail every call for the window with `ErrNotFound`, `ErrCorrupt`, `ErrMaterialUnavailable` or a local error instead; assert the store stays healthy. |
| [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports) throughput floor | Trickle a put below the floor; assert it fails within the interval. Stall a reader of a fetch instead; assert the transfer does not fail. |
| [§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch) joining | Leave the loop as the first of three joined callers; assert the others complete. Fail the fetch; assert all callers see the error and the next demand starts a fresh fetch. Join a speculative fetch with a demand; assert it is promoted. Join after the needed chunk has passed; assert a new fetch. Stop reading as one caller; assert it is detached within the bound and the others continue. |
| [§1.3](#1.3%20Interface) lifecycle | Call on a closed flow and after `Close`; assert both fail. Assert `Close` returns only when transfers have ended, and that `src` is never called after `Upload` returns. |
| [§1.3](#1.3%20Interface) stream rules | Assert an error is yielded once and ends the stream, and that leaving the loop stops the store's read. |
| [§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows) store level | Run eight flows on a slow store and one on a healthy one; assert the healthy store's flow starts within one round and the slow store never holds more than its cap. |

### 7.3 What must not stand in

- **A backend that returns instantly MUST NOT be used for [§2](#2.%20What%20both%20halves%20obey) or [§4.4](#4.4%20Speculation%20does%20not%20delay%20demand).** With no
  latency the pool is never the binding constraint, so every check in those
  sections passes without exercising what it names.
- **A lifetime count MUST NOT be the source for a store's health.** Health is the
  latest probe, or a window of failures since it ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)); a since-start rate
  never lets a recovered store count as healthy.

### 7.4 Benchmarks

Conformance says the syncer is correct; these say what it costs. They use the
real clock — a virtual clock makes time free, the opposite of what a benchmark
wants — and a fake store with a simulated link: a per-call latency and a
per-byte time, never zero ([§7.3](#7.3%20What%20must%20not%20stand%20in)). Each reports throughput, allocations and
peak memory, and a result is recorded with the revision it measured.

| # | Measures | Setup | Expect |
| --- | --- | --- | --- |
| B1 | the syncer's own overhead | the same pool size, once through the syncer and once as raw calls on the fake store | throughput within a few percent of raw; no allocation per chunk once warm, buffers being reused |
| B2 | scheduler cost | one transfer dispatched with 1, 10, 100 and 1,000 flows waiting | flat: DRR is O(1) per dispatch ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)) |
| B3 | fairness under load | one flow saturating uploads on a slow store; a second issuing fetches | the second flow's p99 wait within one round; the first never above its cap |
| B4 | joined fetches | N readers on one cold chunk | one transfer, whatever N ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) |
| B5 | the real link | the syncer over a local emulator of the remote service and over a real service, at the pool sizes the sizing tool printed ([§2.11](#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)) | within a few percent of the tool's raw figure; the gap is what the syncer costs on a real link |

Benchmarks run after a change merges and never gate one; what gates a change
instead is in [§8.4](#8.4%20What%20CI%20checks%20instead%20of%20timing). B5 needs a real store and credentials, and runs where those are.

B5 reports a fraction of the sizing tool's raw figure, with the pools' occupancy
beside it: a low fraction with a pool that is not full means the limit is
upstream of the syncer ([RFC 8 §13.5](rfc-8-engine.md#13.5%20Benchmarks)).

## 8. Test plan and performance targets

[§7](#7.%20Conformance) says what must be checked, and [§1.4](#1.4%20It%20is%20testable%20on%20its%20own) what a check is built from. This
section is the plan around it.

### 8.1 Kinds of test

| Kind | What it covers | How |
| --- | --- | --- |
| Conformance | every check in [§7](#7.%20Conformance), Groups A and B | the fault-injecting fake store of [§1.4](#1.4%20It%20is%20testable%20on%20its%20own), virtual time, race detector |
| Real backend | what the fake cannot fail like ([§7.3](#7.3%20What%20must%20not%20stand%20in)) | nightly: the Group A checks and the lifecycle checks against a local emulator of the remote service, with a fault proxy between them |
| Scheduler model | S17 and S18 over arrivals no hand-written case thinks of | random flows, block sizes and arrival times on the virtual clock, checked after every dispatch against a reference deficit-round-robin: who runs next, and that no flow or store exceeds its cap. A failing run is shrunk and kept |
| Memory | S2 | held-pool heap checks ([§1.4](#1.4%20It%20is%20testable%20on%20its%20own)) |
| Structure | the dependency set of [§1.4](#1.4%20It%20is%20testable%20on%20its%20own) | an import test |
| Benchmark | [§7.4](#7.4%20Benchmarks), B1–B5 | real time; B1–B4 on a fake with a simulated link, B5 on a real store; after merge, never gating ([§8.4](#8.4%20What%20CI%20checks%20instead%20of%20timing)) |
| Soak | leaks and drift | hours, nightly, against a local emulator of the remote service with outages injected on a cycle, asserting tasks, open connections, `inflight_bytes` and queue depth return to idle after each |

There is no crash test here. The syncer persists nothing (S7), so a crash leaves
nothing of its own to recover; what a crash does to transfers in flight is the
engine's to test ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)).

### 8.2 Edge cases

The conformance, model and fault tests **MUST** reach these.

**Shape of a transfer**

- a block of one chunk, and one at the largest block size;
- a `src` that yields an error before its first chunk, and after its last;
- `want` empty, one range, every range, adjacent ranges, ranges that are not
  adjacent, the same range twice, and a range ending at the block's last byte;
- a chain that declares every length, and one that needs a measuring pass.

**Timing of a call**

- `ctx` cancelled while queued, while transferring, and after the store answered
  but before the call returned;
- `Flow.Close` with transfers queued and running, and `Close` while a `Prefetch`
  is joined by a demand;
- a store that turns unhealthy while transfers to it are running.

**Scale of the scheduler**

- a pool of one, where the cap rules meet ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed));
- a thousand flows, one of which has work;
- a flow opened and closed without ever transferring.

**Probes**

- a probe that never returns, which counts as a failure ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)).

### 8.3 Faults and outside interference

Faults are injected systematically: the fake store fails the *n*th call of a
scripted run, for every *n*, with each failure kind of [§1.4](#1.4%20It%20is%20testable%20on%20its%20own) — transient,
terminal, committed with the response lost, stopped part-way, corrupted, held.
After each, every transfer has ended with a report (S4), nothing unconfirmed was
reported durable (S5, S6), and no chunk that failed verification was yielded
(S10).

Against the real backend, a fault proxy adds what the fake cannot: a connection
reset mid-body, a bandwidth cap below the throughput floor, throttling, and
a response dropped after the store committed.

The store can also change behind the syncer, and the tests cover what it then
sees:

| Change | What the syncer does |
| --- | --- |
| a block deleted | `ErrNotFound`; the engine re-resolves ([RFC 8 §6.7](rfc-8-engine.md#6.7%20An%20absent%20object%20is%20re-resolved%20exactly%20once)) |
| a block overwritten with other bytes | `ErrCorrupt` |
| the bucket removed, or credentials revoked | terminal failures; the probe fails and the store turns unhealthy |
| the store slowed below the floor | transfers fail on the floor and are retried within the bound |

### 8.4 What CI checks instead of timing

What gates a change is only the counts below, on the virtual clock and against
the fake, with the same answer on any machine. Each is the count behind one
benchmark:

- **allocations**: none per chunk once warm (B1);
- **scheduler work per dispatch**: queue operations counted at 1, 10, 100 and
  1,000 waiting flows **MUST** be the same (B2);
- **fairness**: bytes dispatched per flow within one quantum, and no flow or
  store above its cap (B3);
- **single flight**: one `Get` for N readers of one cold chunk (B4);
- **peak in flight** never above the pool size (S1).

### 8.5 Performance targets

B1 is stated against raw calls on the same fake, and B5 against the sizing
tool's raw figure for the same store, so both hold on any link. Results are
recorded in absolute numbers.

| # | Metric | Proposed target |
| --- | --- | --- |
| B1 | throughput through the syncer, same pool size | ≥ 97% of raw calls on the fake |
| B2 | one dispatch with 1,000 flows waiting | ≤ 1 µs, and flat from 1 flow |
| B3 | a fetch's queue wait while another flow saturates uploads | p99 within one round |
| B5 | throughput through the syncer on a real store | ≥ 95% of the sizing tool's raw figure; below 90% is a regression |
| — | peak memory per half | ≤ pool × the per-transfer figure of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound), plus 10% |
| — | a failed store refused | within one probe interval plus the probe's bound |
| — | a recovered store accepting work | within one probe interval |

B4 is a count, not a speed, and is fully checked in CI.

### 8.6 Recording results

Beside what [the RFC index](rfc-index.md#Test%20tiers) asks every result to record: the backend and its
region, round-trip time, bandwidth, block size, the pool sizes and the sizing
tool's raw figures; for B1–B4, the fake's simulated latency and per-byte time.

## 9. Open questions

1. **A joint cap for the two pools.** Whether the halves contend enough on a
   shared link to need one cap rather than two is unmeasured.
2. **Probe flapping** ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)). One failed probe makes a store unhealthy and one
   success makes it healthy. Whether a store that fails intermittently needs a run
   of failures before turning unhealthy depends on how often real probes fail
   spuriously, which is unmeasured.
3. **The fixed values** ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)). The per-flow cap, queue length, probe interval
   and the two pool defaults are reasoned, not measured. The cap trades a flow
   alone on the system (wants it high) against the wait of a second flow's first
   transfer when the first flow's store is slow (wants it low).
4. **Considered and deferred.** Each waits for a measurement that asks for it:
   hedged cold reads ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)); a bandwidth cap per direction, which [§2.1](#2.1%20A%20worker%20pool%20is%20the%20only%20concurrency%20control)
   permits only with its interaction with the pool stated; spreading connections
   across the service's addresses.

## 10. Deviations

Where the current implementation departs from this document, one line per
requirement. Each is a change to make, not a question to answer. Deviations of
the remote store itself (retries in the client, a fixed connection limit, put
integrity) are listed once, in [RFC 4 Appendix A](rfc-4-remote-tier.md#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs).

| # | Requirement | Implementation today |
| --- | --- | --- |
| D1 | one syncer per process ([§1.3](#1.3%20Interface)) | one syncer per share |
| D2 | two process-wide pool settings ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) | an upload window per store, adaptive by default |
| D3 | `fetch_workers` is a setting ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) | derived from the CPU count and not settable |
| D4 | one probe interval, one failure turns unhealthy ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) | two intervals, three failures, and a separate demand timeout |
| D7 | the uploader streams a journal reference ([§3.2](#3.2%20It%20holds%20a%20reference%2C%20not%20a%20copy)) | the block is copied into memory before a slot is free, and again for the put |
| D8 | a failed fill does not fail the read ([§4.2](#4.2%20The%20reply%20neither%20waits%20on%20the%20fill%20nor%20fails%20with%20it)) | a fill error fails the fetching caller and every joined one |
| D9 | a retry after an unknown outcome writes the same name ([§2.5](#2.5%20An%20unknown%20outcome%20is%20not%20a%20success)) | block names are random, so a retry leaves an orphan |
| D10 | one pool bounds fetches in flight ([§2.1](#2.1%20A%20worker%20pool%20is%20the%20only%20concurrency%20control)) | each read and warm run builds its own fetch group |
| D11 | one caller leaving does not cancel a joined fetch ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) | the shared fetch runs on the first caller's context |
| D12 | fetches are joined per chunk ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) | joined per chunk and starting offset |
| D13 | pre-warm never drives the journal to refusing writes ([§4.5](#4.5%20Speculation%20is%20executed%20here%20and%20decided%20elsewhere)) | a warm run fills until the journal is full |
| D14 | a transient failure triggers a probe; logs and refusals name the store ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) | ticker-only probing; unhealthy logs and refusals carry no store name |
| D15 | everything but the pool sizes is fixed and derived ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) | further hard-coded queue lengths and timeouts in place of a throughput floor |
| D16 | `Close` returns once every transfer has ended ([§1.3](#1.3%20Interface)) | waits give up after a timeout; reader-side fetches are untracked |
| D17 | the syncer decides and persists nothing ([§1.1](#1.1%20Non-goals)) | the upload side decides when to carve and commits block records itself |
| D18 | no store is declared non-durable ([§2.6](#2.6%20Durability%20is%20observed%2C%20never%20inferred)) | a per-store durability setting, and a commit rule that branches on it |
| D20 | fair scheduling across stores and flows ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)) | no per-flow queue, round robin or cap |
| D21 | a closed `Store` error set, a streamed put, and the census returned by `Put` ([§1.3](#1.3%20Interface)) | none of these exists; callers interpret service errors |
| D22 | GC relocates through a syncer flow ([§1.3](#1.3%20Interface)) | relocation calls the remote store directly |
