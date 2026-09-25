---
rfc: 3
title: "RFC 3 — the syncer"
component: syncer
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-8-remote-tier]]"
aliases:
  - RFC 3
tags:
  - rfc
---
# RFC 3 — the syncer

**Status:** draft.
**Depends on:** [RFC 0](rfc-0-data-lifecycle.md), for the terms and the residency function. [RFC 8](rfc-8-remote-tier.md) specifies
the remote tier this component calls. [RFC 1](rfc-1-journal.md) supplies and receives the bytes.
[RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block) names the blocks.
**Audience:** anyone changing the component that moves chunks between the journal
and the remote tier.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are
to be interpreted as in RFC 2119.

---

## In short

- The syncer moves chunks between the journal and the remote tier: out as whole
  blocks, back as the chunks a read needs. It does nothing else.
- It has two halves. The **uploader** puts boxed blocks out, one object each (a block is *boxed* once
  the engine has packed its chunks, [RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component)). The
  **fetcher** gets remote-only chunks back, as a range of their block or as the
  whole block.
- Each half is a worker pool of configurable size. That pool is the only
  concurrency control, and its size is the memory bound.
- The uploader holds a *reference* into the journal, not a copy. The journal keeps
  those bytes stable until the upload reports.
- The fetcher hands verified chunks to its caller, the engine, which answers the
  waiting read and may store them in the journal (a *fill*).
- It names nothing, deletes nothing, decides nothing, and stores nothing.

---

## 1. Purpose

> **Move chunks between the journal and the remote tier, in both directions,
> under a bound: out as whole blocks, back as the chunks that are needed.**

The two directions are deliberately asymmetric. A block is
the unit of **storage**: one object, one name, written by one put, so it goes out
whole. A chunk is the unit of **verification**: each carries its own hash, so any
chunk can be read and checked on its own. A fetch therefore asks for the chunks it
needs — a range of the block — or for the whole block when most of it is wanted
([RFC 6 §6.8](rfc-6-engine.md#6.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block)).

Every transition to **Resident** in this system originates in an outcome this
component reports ([RFC 0 §4.3](rfc-0-data-lifecycle.md#4.3%20Reporting)). Every transition back from **Remote** to holding
local bytes originates in a fetch it performed.

### 1.1 Non-goals

The syncer **MUST NOT**:

- decide *what* to transfer, or *when*, or *why* — that is flush, eviction and
  read-ahead policy ([RFC 0 §5.2](rfc-0-data-lifecycle.md#5.2%20Flush), [§8.1](rfc-0-data-lifecycle.md#8.1%20Evict), [RFC 6](rfc-6-engine.md));
- decide what to delete — that is sweep, which calls the remote tier directly
  ([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep), [RFC 7](rfc-7-gc.md));
- derive a block's name ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block));
- know how an object is framed, transformed or verified ([RFC 8 §3](rfc-8-remote-tier.md#3.%20The%20stored%20object), [§4](rfc-8-remote-tier.md#4.%20The%20transform%20chain), [§6](rfc-8-remote-tier.md#6.%20Reads%20are%20verified%20at%20this%20boundary));
- persist anything;
- know what a file, an [extent](rfc-0-data-lifecycle.md#2.1%20Entities) or a [segment](rfc-0-data-lifecycle.md#2.1%20Entities) is, or which files a block's
  [chunks](rfc-0-data-lifecycle.md#2.1%20Entities) came from. To the syncer a chunk is a hash and its bytes, the unit it
  verifies and streams; where chunk boundaries fall, what refers to a chunk, and
  how chunks were grouped into a block are not its concern.

### 1.2 Two halves, one component

| | uploader | fetcher |
| --- | --- | --- |
| Triggered by | a block finished boxing | a read of bytes held only remotely |
| Moves | one whole block per transfer | the chunks asked for, as a range of one block, or the whole block |
| Reads from | the journal, by reference | the remote tier |
| Writes to | the remote tier | its caller, which answers the read and may fill the journal |
| Limited by | its own worker pool | its own worker pool |
| Work is | known in advance, and unique | demanded, and may be duplicated |

The halves share a component and share nothing else: no pool, no queue, no
counter. Their sizes **MUST** be configurable independently, because they saturate
on different things — uploads on the uplink and on how fast blocks are boxed,
fetches on latency and on how many readers are blocked.

They are one component because they are one obligation seen from two sides:
bounded movement of chunks, grouped into blocks, with the journal at one end and [RFC 8](rfc-8-remote-tier.md)'s contract
at the other. Splitting them would duplicate [§2](#2.%20What%20both%20halves%20obey) into two documents.

### 1.3 Interface

Signatures are indicative; the obligations are normative.

```go
type Chunk struct {
    Hash  Hash   // plaintext content hash
    Bytes []byte // borrowed until the next iteration
}

// Range is where one chunk's record sits in a stored object: a byte offset and a
// length in the object as stored, after framing and transforms. The store
// reports it on Put; the syncer passes it through and reads only Len.
type Range struct {
    Off, Len int64
}

// ChunkRange asks for one chunk: the hash to verify it against, and its range.
type ChunkRange struct {
    Hash  Hash
    Range Range
}

// Store is what the syncer needs from a backend, declared here and named for
// that need. The engine adapts the remote tier (RFC 8) to it.
type Store interface {
    // Put stores the block and returns one Range per chunk, in the order given.
    Put(ctx context.Context, name BlockName, chunks iter.Seq2[Chunk, error]) ([]Range, error)
    // Get reads the chunks named in want, or the whole block when want is empty.
    // Ranges adjacent in the object are read with one request. Each chunk comes
    // back verified against its own hash.
    Get(ctx context.Context, name BlockName, want []ChunkRange) iter.Seq2[Chunk, error]
    Probe(ctx context.Context) error
}

Register(name string, store Store) (StoreID, error)
OpenFlow(store StoreID) (*Flow, error)

func (f *Flow) Upload(ctx context.Context, name BlockName, size int64, src func() iter.Seq2[Chunk, error]) ([]Range, error)
func (f *Flow) Fetch(ctx context.Context, name BlockName, want []ChunkRange) iter.Seq2[Chunk, error]
func (f *Flow) Prefetch(ctx context.Context, name BlockName) iter.Seq2[Chunk, error]
func (f *Flow) Healthy() bool
func (f *Flow) Close() error

Close() error // the syncer's own
Stats() Stats // a consistent snapshot of the counters of §2.12
```

Streams are the standard library's `iter.Seq2`: a caller consumes one with
`for c, err := range`, and leaving the loop early stops the producer.

`Chunk` and `Store` are declared here, in the syncer's own package, as
[RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy) requires: the syncer imports neither the carver, the journal nor the
remote tier, and is tested with none of them ([§1.4](#1.4%20It%20is%20testable%20on%20its%20own)). `Chunk` is not the
carver's chunk ([RFC 2 §1.2](rfc-2-carver.md#1.2%20Two%20layers%3A%20the%20chunker%20and%20the%20carver)) under another name. The carver's describes where a
chunk sits in a file — offset, length, hash — and hands the bytes separately; this
one is what a transfer carries, a hash and its bytes, and has no file offset. The
engine converts one into the other. `Hash` is a plain `[32]byte`, so not even the
hash type is shared by import. `Store` has no delete: the syncer never deletes
([§1.1](#1.1%20Non-goals)).

**There is one syncer per process.** An installation may configure several
block stores — several buckets, say — and each is registered once. Work reaches
the syncer through a **flow**: a handle bound to one store and to one queue in the
fairness scheduler ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)). A transfer on a flow goes to that flow's store and
waits in that flow's queue, so a transfer cannot be sent to the wrong store, and
a call carries neither a store nor a key. `Flow.Close` takes the flow out of
the scheduler; a call on a closed flow **MUST** fail.

**The syncer does not know what a flow stands for.** It knows stores, flows and
blocks, and nothing about shares, files or tenants. Choosing what is fair against
what is the engine's: it opens one flow per share, on that share's store, and
closes it when the share is removed ([RFC 6](rfc-6-engine.md)). Fairness per tenant, or a separate
flow for pre-warm so it cannot crowd out its own share's reads, would change
which flows the engine opens and nothing here. The engine's share-level code sees
a flow only through an interface the engine declares for its own need
([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)).

One syncer is what the bounds need: the memory bound is a property of the process
([§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), and a store's health is a property of all the traffic it carries,
from every flow on it ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)). A syncer per share would state neither.

![Three flows, each bound to one store: two queue their transfers per flow, a deficit-round-robin scheduler with a per-flow cap feeds the uploader and fetcher pools, and the pools reach two stores; the third flow's store is unhealthy, its probe keeps running, and its calls are refused before they queue](img/rfc3-overview.svg)

`name` is the block's name ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)). With `want` it is everything
[RFC 8 §6.1](rfc-8-remote-tier.md#6.1%20The%20exported%20read%20takes%20the%20expected%20hash)'s verified read needs. The syncer passes both through and **MUST NOT**
interpret them, except to sum the lengths of `want` for scheduling ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)).

**Both directions stream, one chunk at a time.** The object format makes this
possible: one record per chunk, each carrying its own plaintext hash and each
readable on its own ([RFC 8 §3.3](rfc-8-remote-tier.md#3.3%20What%20the%20format%20must%20guarantee), F1, F2, F5), and every transform applies per
chunk ([RFC 9](rfc-9-transforms.md)). A worker therefore never needs the whole block in memory, which
is what sets the bound of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound).

**`Upload`** puts the block whose chunks `src` yields from the journal
([§3.2](#3.2%20It%20holds%20a%20reference%2C%20not%20a%20copy)). It hands that stream to the store's `Put`, which frames and transforms
each chunk beneath the syncer; the syncer never sees a transformed byte
([RFC 8 §4.4](rfc-8-remote-tier.md#4.4%20The%20syncer%20MUST%20NOT%20observe%20that%20a%20transform%20happened)). `size` is the block's plaintext length, which the engine knows from
its plan and the scheduler charges ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)).

It waits in its flow's queue until a worker is free or `ctx` ends, which is the
backpressure of [§2.3](#2.3%20Backpressure%20propagates%3B%20it%20does%20not%20buffer), and fails at once if its store is unhealthy ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) or its
flow's queue is full ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)). It returns the store's `Range` for each chunk, and a
`nil` error, only on the store's acknowledgement, which is durable because every
store is ([§2.6](#2.6%20Durability%20is%20observed%2C%20never%20inferred)); every other ending, an unknown one included
([§2.5](#2.5%20An%20unknown%20outcome%20is%20not%20a%20success)), is an error. The ranges go into the block's commit, where later reads
find them ([RFC 4 §2.2](rfc-4-block-metadata.md#2.2%20Chunk)).

A retry calls `src` again for a fresh stream from the first chunk, so the bytes
behind it **MUST** stay stable until `Upload` returns ([§3.3](#3.3%20The%20journal%20keeps%20referenced%20bytes%20stable)). It **MUST NOT**
call `src` after it returns, which is what lets the caller hand the journal
reference back.

**`Fetch`** is a demand: a reader is waiting. **`Prefetch`** is speculation
([§4.4](#4.4%20Speculation%20does%20not%20delay%20demand)): nobody is waiting yet. They are separate methods, not one method with
a priority parameter, so each call site shows which it means. `Prefetch` always
fetches the whole block: speculation is read-ahead or pre-warm, and both want
whole blocks ([RFC 6 §6.8](rfc-6-engine.md#6.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block)).

**A fetch is a set of whole-chunk ranges of one block.** `want` names the chunks
the caller needs, each by its hash and the `Range` the store reported when the
block was put, which block metadata keeps ([RFC 4 §2.2](rfc-4-block-metadata.md#2.2%20Chunk)). An empty `want` asks for the
whole block; each record is then verified against its own hash, and the ordered
hashes against `name`, from which [RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block) derives it. The store merges ranges
adjacent in the object into one ranged read — on S3, one `GET` with a
`Range: bytes=first-last` header — so a packed small file, or a run of one file's
chunks, costs one request ([RFC 6 §5.7](rfc-6-engine.md#5.7%20A%20block%20packs%20chunks%2C%20whichever%20files%20they%20came%20from)); ranges that are not adjacent cost
one request each, or the engine asks for the whole block instead. A range is
always a whole chunk: a range that splits one has no hash to check it against
([§4.1](#4.1%20One%20fetch%2C%20two%20consumers)).

A cold random read asks for only the chunks it covers, and each comes back as its
own verified range: fetching a 20 MiB block to serve one 4 KiB read is read
amplification with no correctness benefit. When to widen a request from chunks to
the whole block is the engine's policy ([RFC 6 §6.8](rfc-6-engine.md#6.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block)): chunks for a read that is small
and not sequential, the whole block for a scan or a read that needs most of it.

A range whose bytes fail verification is reported as a **range mismatch**, distinct
from a corrupt object, so the engine can re-resolve once ([RFC 6 §6.7](rfc-6-engine.md#6.7%20An%20absent%20object%20is%20re-resolved%20exactly%20once)): a re-put under
the same name may lay the object out differently ([RFC 8 §5.5](rfc-8-remote-tier.md#5.5%20A%20put%20of%20an%20existing%20key%20succeeds%3B%20so%20does%20a%20delete%20of%20an%20absent%20one)), and a range recorded
before it then points at the wrong bytes.

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

`Store` has no health method, only `Probe`: the syncer probes each store itself
and refuses work for an unhealthy one, and `Flow.Healthy` reports the result
([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)).

### 1.4 It is testable on its own

The syncer's dependencies are exactly four, and each is supplied, never reached
for:

| Dependency | Supplied as | In a test |
| --- | --- | --- |
| a backend | the `Store` interface of [§1.3](#1.3%20Interface) | a fault-injecting fake |
| the bytes to upload | the `src` closure passed to `Upload` | a closure over a byte slice |
| time | the Go runtime clock | `testing/synctest`, which runs the clock virtually |
| logging | a `*slog.Logger` | a handler that records the lines |

No journal, no carver, no metadata store, no engine, no disk and no network.
A conformance check **MUST** be writable as: build a syncer over a fake store,
call it, assert on what the fake saw and what came back. An implementation that
picks up another dependency loses that, and **MUST NOT**.

**Time is virtual in tests.** The probe interval, the unhealthy log interval,
retry backoff and every `ctx` deadline run inside a `synctest` bubble, where time
advances only when every goroutine is blocked. A check of the 60-second log line
([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) runs in microseconds and gives the same answer every time. The syncer
therefore **MUST NOT** read time from anywhere a bubble cannot fake — no clock
handed in from outside, no timer owned by another package. A check that sleeps on
the real clock is slow, and on a loaded machine it fails for reasons that have
nothing to do with the syncer.

**The fake store is the fixture every check shares.** Per call it can:

- take a set latency and a set time per byte, so the pool binds ([§7.3](#7.3%20What%20must%20not%20stand%20in));
- fail transiently or terminally, once or every time;
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
against MinIO in a container ([§7.3](#7.3%20What%20must%20not%20stand%20in)).

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

Each worker runs one transfer at a time, and a streaming transfer holds only the
record it is working on: one chunk, transformed and framed ([§1.3](#1.3%20Interface)). A chunk is
at most `Max` ([RFC 2](rfc-2-carver.md), B3), so:

`peak memory of a half = pool size × (chunk Max + one record's framing)`

That holds only if nothing on the path buffers the whole object. Where a stage
does — a backend SDK that needs the body in memory to sign it — the
per-transfer figure is the largest block instead, the block target plus one chunk
([RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component), P2). An implementation **MUST** state which applies to each backend it
uses, and **MUST** be able to state the resulting number for its configuration,
because it is what an operator sizes the pools from ([§9](#9.%20Open%20questions)). The whole process's
bound is the uploader's number plus the fetcher's.

The limit belongs to the syncer, not to each store, because memory runs out for
the whole process, not for one store. A limit set per store cannot bound the
process. Several shares can use the same store, so its limit would have to cover
all their traffic together, and one share's traffic can go to more than one
store, so no single store's limit covers it. Only a limit set where all the
transfers happen, in the syncer, adds up to a number the process can hold.

### 2.3 Backpressure propagates; it does not buffer

When a pool is full, the caller **MUST** wait or be refused. An implementation
**MUST NOT** accept work into an unbounded queue, and **MUST NOT** introduce a
buffer whose size is not configured.

The chain this preserves runs the whole depth of the system:

remote slows → the pool saturates → flush stalls → dirty [extents](rfc-0-data-lifecycle.md#2.1%20Entities) are not released
→ the journal approaches capacity → writes are refused ([RFC 0 §10.1](rfc-0-data-lifecycle.md#10.1%20Capacity%20is%20a%20bound%2C%20not%20a%20target))

Refusing a write is the specified outcome. An unbounded queue at any link replaces
it with unbounded memory growth, and the process dies instead of declining work.

![The backpressure chain from a slowing remote through to a refused write, and the same chain with one unbounded queue inserted, which ends in the process dying instead](img/rfc3-backpressure-chain.svg)

### 2.4 Every transfer terminates, and reports

Every transfer **MUST** complete in bounded time with either success or a failure
reported to its caller. No transfer retries indefinitely.

An implementation **SHOULD** distinguish transient failures (timeout, throttle,
connection reset) from terminal ones (malformed request, rejected credentials,
absent bucket) and retry only the former, within a stated bound. Misclassification
either way is recoverable, because both paths end in a report. A throttling
response — S3's `503 SlowDown`, returned while it scales a prefix past its
request rate — is transient.

**Bounded time is enforced by throughput, not by a fixed deadline.** A fixed
per-transfer timeout is too short for a maximum-size block on a slow link and far
too long for a connection that has stopped moving. A transfer **MUST** instead
fail when its throughput stays below a stated floor for a stated interval, the
rule curl applies as `--speed-limit` / `--speed-time`. The floor is fixed
([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) at 64 KiB/s over 30 s: below any working link, and high enough that a
maximum-size block cannot crawl for more than about five minutes. The AWS Common
Runtime S3 client's default, 1 byte per second over 30 s, catches a stalled
connection but not a crawling one. Time to the first byte is bounded separately,
since a request can stall before any byte moves.

Only the store's side counts. Time a caller spends not reading a fetched stream is
the caller's, and never trips the floor: a consumer that stops reading is
[§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)'s to handle, by detaching it, not a reason to fail the transfer for every
caller joined to it.

**A slow tail is retried, not waited out.** Following AWS's guidance for S3
("aggressive timeouts and retries help drive consistent latency"), a transfer
whose first byte is later than a bound derived from recent latency — the AWS
Common Runtime client starts from the 90th percentile — **SHOULD** be cancelled and
retried on a new connection, within the retry bound. Sending a second request
alongside the first and taking whichever answers ("hedging", Dean and Barroso,
*The Tail at Scale*, 2013) cuts tail latency further at a few percent more
requests; it is deferred until a measurement of cold-read latency asks for it.

**The syncer is the only layer that retries.** A backend's client makes one
attempt per call and returns its error ([RFC 8 §5.7](rfc-8-remote-tier.md#5.7%20A%20backend%20holds%20no%20state%20that%20spans%20operations); today's code differs,
[RFC 8 §5.7.1](rfc-8-remote-tier.md#5.7.1%20Deviation%20%E2%80%94%20the%20SDK%20retries%20inside%20the%20store)). Retrying needs state across operations, and that state
is the syncer's ([RFC 8 §2.1](rfc-8-remote-tier.md#2.1%20The%20rule%3A%20one%20operation%2C%20or%20many)); two layers
that each retry multiply into a number of attempts nobody stated. A streamed body
also cannot be rewound by a client that has already sent part of it: only the
syncer, which can call `src` again ([§1.3](#1.3%20Interface)), can retry one.

### 2.5 An unknown outcome is not a success

A transfer can end with its outcome genuinely unknown: the request was sent, the
response was lost. The syncer **MUST** resolve that as *not durable* and report it
as a failure.

It happens on every network backend. A put is sent and S3 writes the object, then
the connection resets, or the client's timeout fires, before the response
arrives. The object exists and the syncer cannot know it. Equally, the request
may have died on the way out and nothing exists. Both look the same from the
client.

This is safe only because a block's name is a function of its content ([RFC 2](rfc-2-carver.md)
[§4.2](rfc-2-carver.md#4.2%20A%20block)), which makes the retry idempotent — the same bytes to the same name,
indistinguishable from having written them once.

The idempotence covers a retry of the same block: `Upload` retrying, or the engine
re-offering the same plan. A later pass that groups the chunks differently derives
a different name, and an object the earlier attempt may have written is then an
orphan until garbage collection finds it ([RFC 7](rfc-7-gc.md)). The engine keeps the plan of
any block whose outcome was unknown and offers it again, unchanged, before
packing anything new ([RFC 6 §5.1](rfc-6-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)), so that is the exception rather than the routine.

![A put whose response was lost leaves three indistinguishable remote states; a retry under the same content-derived name converges all three to one object](img/rfc3-unknown-outcome.svg)

![Three orders of naming and recording a block, each crashed at its worst moment: a key recorded before the put leaves a dangling reference, a put before the record leaves an orphan only a listing can find, and a key derived from the content makes the retry write the same object](img/rfc3-key-derivation.svg)

### 2.6 Durability is observed, never inferred

The syncer **MUST** report durability only on the acknowledgement [RFC 8 §5.6](rfc-8-remote-tier.md#5.6%20What%20acknowledgement%20means%20is%20the%20backend%27s%20to%20declare)
defines for the backend in use. None of the following is evidence, and an
implementation **MUST NOT** report durability on any of them:

| Not evidence | Why it looks like evidence |
| --- | --- |
| the call returned | it returned from a buffer, not from storage |
| no error was observed | an error that is never delivered is not an absence of error |
| the bytes were all sent | sent is not stored |
| enough time has passed | time is not an acknowledgement |
| a later read succeeded | the read may be served from a cache the write populated |

**Every store is durable.** A store holds the only copy of data the journal has
released, so a store that can lose an acknowledged object is not one this design
admits: there is no non-durable store, and no setting that declares one. What
counts as the acknowledgement is fixed per backend ([RFC 8 §5.6](rfc-8-remote-tier.md#5.6%20What%20acknowledgement%20means%20is%20the%20backend%27s%20to%20declare)):

| Backend | Acknowledgement |
| --- | --- |
| AWS S3 | a successful `PutObject` response: S3 answers only once the object is stored redundantly, and a read after it sees the object |
| S3-compatible services | the same response; a service that does not promise redundant storage on it **MUST NOT** be configured as a store |
| in-memory store | a test fixture, not a store an installation can configure; it acknowledges a put once it holds the bytes, so tests exercise the same path as production |

### 2.7 It reports; it does not persist

The syncer **MUST NOT** persist durability, residency or health. It returns what
it observed; [RFC 4](rfc-4-block-metadata.md) records it and [RFC 1](rfc-1-journal.md) is told ([RFC 0 §4.3](rfc-0-data-lifecycle.md#4.3%20Reporting)).

A syncer that kept its own durable record of what it moved would be a third
record beside block metadata ([RFC 4](rfc-4-block-metadata.md)) and the journal. After a crash it can
disagree with both, and nothing in the
residency model resolves a three-way disagreement.

### 2.8 An unhealthy store refuses work

A store's health belongs to the store. Every backend exposes a liveness probe —
one round trip, no state ([RFC 8 §5.8](rfc-8-remote-tier.md#5.8%20The%20liveness%20probe%20is%20one%20operation)) — and the syncer calls it to decide
whether the store is **healthy** or **unhealthy**:

- the syncer **MUST** probe every registered store at a configured interval, and
  **SHOULD** probe at once when a transfer to it fails transiently ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports));
- a failed probe makes the store unhealthy; a successful one makes it healthy;
- a store also turns unhealthy when every transfer to it failed transiently over
  a stated window ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) although its probe passed — a store that answers the
  probe but throttles or refuses puts, as S3 does with `503 SlowDown` on a busy
  prefix or `403` after a bucket-policy change. It turns healthy on the next
  successful probe, so such a store is retried once per probe interval rather
  than continuously, and never latched;
- an unhealthy store **MUST** keep being probed. Once transfers stop, the probe
  is the only thing that can observe recovery, so without it unhealthy would be
    the latch [§5](#5.%20What%20belongs%20elsewhere) forbids;
- a probe **MUST** be bounded in time like any call, and one that does not
  return within its bound is a failed probe. A hung probe that left the store
  healthy would keep sending traffic to a dead store.

**An unhealthy store refuses work.** `Upload`, `Fetch` and `Prefetch` targeting it
**MUST** fail at once, before taking a worker and without calling the backend.
Retrying a request the syncer already knows will fail spends a worker, a round
trip and a timeout to learn nothing, and a pool full of such retries stalls every
flow on every other store. The refusal is cheap and certain; the probe, not the
traffic, is what finds out when the store is back.

A transfer already running when the store turns unhealthy is left alone. It
succeeds or fails on its own, within [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)'s bound; nothing cancels a put
mid-flight.

`Flow.Healthy` reports the current state of the flow's store. It reads memory and never calls the
backend, so a caller can check it before doing work that would end in a refused
upload — the engine skips carving and packing a flush for a store it would
refuse ([RFC 6 §8.1](rfc-6-engine.md#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20flush%20included)). The state is re-derived from the next probe, so it is not
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

The design is taken whole from two sources rather than invented:
**Deficit Round Robin** (DRR; Shreedhar and Varghese, SIGCOMM 1995), the scheduler Linux
traffic control ships as `tc-drr`, and the queueing around it from **Kubernetes API
Priority and Fairness** (APF, KEP-1040), which solves the same problem: one shared
concurrency pool, many flows, none allowed to starve another. APF's term is
used here for the same reason: a flow is whatever the caller wants kept apart.

Each half runs its own scheduler over its own pool:

- **One queue per flow.** A transfer waits in its flow's queue, never in a
  global one.
- **Turns are in bytes, not requests.** When a worker frees, the scheduler visits
  the next flow with work waiting, adds a **quantum** of bytes to that flow's
  **deficit**, and dispatches from the head of its queue while the head's size
  fits within the deficit, subtracting each one's size. A flow whose queue
  empties has its deficit reset to zero. A transfer is charged its size: an
  upload the `size` it declares, a fetch the sum of its `want` ranges' lengths,
  and a whole-block fetch the largest block ([§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), since its size is unknown. A flow sending large blocks
  therefore gets no more bandwidth than one sending small ones.
- **The quantum is at least the largest block**, so every flow with work
  waiting dispatches at least once per round. Each dispatch costs O(1).
- **Two levels: stores, then flows.** The scheduler first picks a store, by the
  same round robin over stores with work waiting, then a flow on that store.
  Without the store level, eight shares on one slow store hold eight flows' caps
  between them — the whole pool — and the flows on a healthy store starve.
- **Within a flow, demand goes before speculation** ([§4.4](#4.4%20Speculation%20does%20not%20delay%20demand)). The scheduler
  picks the flow; the flow's queue picks the transfer.
- **No store, and no flow, holds more than a fixed cap of the pool's workers at once**,
  stated as a fraction of the pool (after APF's borrowing limit). Fair turns only
  decide who gets the *next* free worker: without the cap, a flow or a store that is
  slow but healthy can hold every worker for as long as its puts take, and no
  turn comes round. The cap is not a reservation. A flow alone on the system uses
  up to the cap, and no worker sits idle waiting for a flow that has no work.
- **Each queue has a configured length.** A call that finds its flow's queue full
  **MUST** be refused, as APF refuses with 429. Waiting outside the queue would be
  an unbounded queue under another name ([§2.3](#2.3%20Backpressure%20propagates%3B%20it%20does%20not%20buffer)). The refusal travels back to
  whoever opened the flow — with one flow per share, that share's journal — and
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
Deriving it from cores would also scale memory with cores — 128 fetchers on a
64-core host is 2 GiB at a 16 MiB largest block, when a stage buffers — while a small VM on a fast link
gets too few.

Each is a memory budget as much as a concurrency limit ([§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), and the
documentation of each **MUST** state the memory it implies at the configured
chunk `Max`. Both are fixed for the life of the process: the pool does not adapt
([§9](#9.%20Open%20questions), question 2).

Everything else the syncer needs is derived or fixed, and **MUST NOT** be a
setting:

| Value | Is | Because |
| --- | --- | --- |
| DRR quantum | the largest block: block target plus chunk `Max` | [§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows) needs it to be at least that, and nothing is gained above it |
| per-store and per-flow cap | three quarters of the pool, rounded down, at least one worker; with a pool of more than one, at most the pool less one | leaves a quarter for every other flow while letting a flow alone use most of the pool |
| per-flow queue length | four times the pool | a waiting transfer holds a reference, not bytes ([§3.2](#3.2%20It%20holds%20a%20reference%2C%20not%20a%20copy)), so the length bounds bookkeeping, not memory |
| probe interval | 5 s, healthy or not | one interval; one failed probe is unhealthy, one success healthy ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) |
| failure window | 30 s | a store whose every transfer failed for this long, with its probe passing, turns unhealthy ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) |
| throughput floor | 64 KiB/s over 30 s, and 10 s to the first byte | [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports) |
| detach bound | 5 s without taking the next chunk | a joined caller that falls behind is detached ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) |
| unhealthy log interval | 60 s | [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)'s summary line |
| retry bound | the syncer's own, stated in code | [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports), [§9](#9.%20Open%20questions) question 5 |

Each fixed value becomes a setting only when a measurement shows the fixed value
is wrong for a workload an operator can name. A setting nobody can reason about
is tuned by trial, and a wrong value shows up as a stall with no stated cause.

Read-ahead depth, pre-warm pacing and when to flush are policy and belong to
[RFC 6](rfc-6-engine.md), not here. Chunk and block sizes belong to [RFC 2](rfc-2-carver.md).

### 2.11 Pool sizes are measured once, by a tool

The pools do not adapt at runtime. The documented way to size them is a separate
tool that measures a store and prints the two settings; the operator writes them
into the configuration, and they hold for the life of the process ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)).
A number printed once can be checked against a real bucket. A controller that
resizes the pool while serving can only be judged by watching it, and its worst
failure — a pool that shrinks during a slowdown — looks like a slow network from
outside.

The tool measures **pure transfer**. It drives the store's own put, get and
delete ([RFC 8 §5.1](rfc-8-remote-tier.md#5.1%20Operations)) with random bytes the size of the largest block, and passes
nothing through the transform chain. It is one implementation on top of the
backend contract, not a method each backend provides: a speed test is many
transfers, and the backend knows only one ([RFC 8 §2.1](rfc-8-remote-tier.md#2.1%20The%20rule%3A%20one%20operation%2C%20or%20many)).

It **MUST**:

- **measure gets twice**: whole blocks, and single-chunk ranges at the chunk
  target, which is what cold random reads issue; small reads are bound by
  latency, so their knee is higher, and the fetch pool takes the larger
  recommendation;
- **measure puts and gets separately**, at doubling concurrency (1, 2, 4, …), and
  take for each the smallest concurrency past which throughput stops rising by a
  stated margin;
- **build the store with a client no smaller than its highest concurrency step**
  ([RFC 8 §5.9](rfc-8-remote-tier.md#5.9%20A%20backend%27s%20client%20never%20queues%20below%20the%20pools)), so what it measures is the link and the service, never
  a queue inside the client;
- **print the pool size, not the knee.** One flow holds at most three quarters
  of a pool ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)), so a flow alone reaches the knee only in a pool of
  at least the knee ÷ 0.75;
- **print the memory each size implies** at the largest block, the upper figure
  of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound),
  and cap the recommendation at a memory budget when one is given;
- **with several stores, measure each and print the largest**, since one pool
  serves them all;
- **write only under a scratch prefix no share uses, and delete what it wrote**,
  including after an interrupted run.

How a run proceeds — step length, warm-up, the stopping rule and the output —
is [Appendix A](#Appendix%20A.%20Running%20the%20pool-sizing%20tool).

Its result describes this host, this link and this time of day. It says so, and
nothing re-runs it automatically.

It does not see CPU. Where compression and encryption cannot keep up with the
printed pool size ([RFC 9](rfc-9-transforms.md)), the pool is larger than the process can use, and
only memory is wasted; measuring the transforms' throughput is RFC 9's.

### 2.12 What the syncer makes observable

The syncer keeps counters, as [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)'s log lines already require, and returns a
consistent snapshot of them from a `Stats` call on the syncer. It imports no
metrics library ([§1.4](#1.4%20It%20is%20testable%20on%20its%20own)); the engine exports the snapshot to Prometheus and to
`dfsctl`'s stats output. Taking a snapshot **MUST NOT** take a lock a dispatch
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
| `dittofs_syncer_bytes_total` | counter | plaintext bytes transferred |
| `dittofs_syncer_transfer_duration_seconds` | histogram | time from dispatch to end |
| `dittofs_syncer_first_byte_seconds` | histogram | time to the first byte; the tail that slow-tail retries and hedging aim at ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) |
| `dittofs_syncer_retries_total` | counter | attempts beyond the first, labelled `reason` = `transient`, `slow_tail` or `floor` ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) |

**Fetches** — are reads being shared and verified?

| Metric | Type | Answers |
| --- | --- | --- |
| `dittofs_syncer_fetch_joined_total` | counter | callers that joined a fetch already in flight ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) |
| `dittofs_syncer_fetch_detached_total` | counter | joined callers detached for falling behind |
| `dittofs_syncer_get_requests_total` | counter | requests sent to the store; against chunks fetched, how well adjacent ranges merge ([§1.3](#1.3%20Interface)) |
| `dittofs_syncer_chunks_fetched_total` | counter | chunks fetched and verified |
| `dittofs_syncer_verify_failures_total` | counter | chunks that failed verification, labelled `kind` = `range_mismatch` or `corrupt` ([§4.1](#4.1%20One%20fetch%2C%20two%20consumers)). Any `corrupt` is an alert |
| `dittofs_syncer_speculation_preempted_total` | counter | speculative fetches that yielded to demand ([§4.4](#4.4%20Speculation%20does%20not%20delay%20demand)) |

**Health** — is each store usable?

| Metric | Type | Answers |
| --- | --- | --- |
| `dittofs_syncer_store_healthy` | gauge (0/1) | the latest probe's verdict ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) |
| `dittofs_syncer_store_unhealthy_seconds_total` | counter | time spent unhealthy |
| `dittofs_syncer_store_transitions_total` | counter | changes of state; a high rate is flapping ([§9](#9.%20Open%20questions), question 9) |
| `dittofs_syncer_probe_duration_seconds` | histogram | time per probe |

Two metrics distinguish situations the logs cannot. `workers_busy` against
throughput tells a slow store from a starved syncer, which is the question the
v0.33.0 benchmark left open at 28% of raw S3 ([§7.4](#7.4%20Benchmarks)). `verify_failures_total`
separates a range that no longer matches its metadata, which re-resolves, from
stored bytes that are wrong, which nothing downstream repairs.

## 3. The uploader


### 3.1 It is triggered, not scheduled

A block becomes the uploader's work when it has finished boxing. The uploader
**MUST NOT** decide which blocks to box, when to box them, or in what order to
take them.

### 3.2 It holds a reference, not a copy

The uploader **MUST** take a reference to the block's bytes in the journal and
read them, chunk by chunk, while a worker transfers the block. It **MUST NOT**
copy the block into memory when the block is queued.

The reference is to the bytes **as offered for flush**, not to the file as it is
now. A client may overwrite an extent while its block waits for a worker
([RFC 1 §3.3](rfc-1-journal.md#3.3%20Flush)); a reference by file offset would then read the newer bytes, whose
hash is not the block's. So the engine describes a block as a plan — its name and,
per chunk, a hash and where its bytes sit in the offered version — and `src` reads
the plan through the journal's `offered` readers, which keeps returning the offered
bytes until the flush callback returns ([RFC 6 §5.1](rfc-6-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)). A block may hold chunks of
several files ([RFC 6 §5.7](rfc-6-engine.md#5.7%20A%20block%20packs%20chunks%2C%20whichever%20files%20they%20came%20from)), so one plan may read through several files'
readers. The syncer sees none of this: to it, `src` is a stream of chunks, and a
block is the same thing whether its chunks came from one file or from a hundred.

The difference is the bound of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound). A copy taken at queue time is held for the
whole time the block waits for a worker, so peak memory follows the queue depth;
a reference is materialised only while a worker is transferring, so it follows the
pool size.

![A block waiting for a worker holds only a plan of chunk hashes and offsets into the offered records; the worker walks the plan through the offered reader, re-hashing each chunk, and streams it to the store's put; a newer record written meanwhile is never read](img/rfc3-upload-plan.svg)

### 3.3 The journal keeps referenced bytes stable

The journal keeps offered bytes stable until the flush callback that offered them
returns ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Flush)): it **MUST NOT** release, overwrite, relocate or compact them
before then. The engine runs the upload inside that callback
([RFC 6 §5.1](rfc-6-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)), so no reference outlives it and `src` is never called after
`Upload` returns.

This is the other half of [§3.2](#3.2%20It%20holds%20a%20reference%2C%20not%20a%20copy) and it is a requirement on [RFC 1](rfc-1-journal.md), not on this
component. A reference into storage that may move underneath it is a copy with
extra steps and a race.

### 3.4 One put per block

A block is transferred by a single put of the whole block ([RFC 8 §5.1](rfc-8-remote-tier.md#5.1%20Operations)). A put that
does not complete **MUST NOT** leave the block retrievable and **MUST NOT** be
reported durable.

Where a backend's client splits a put into parts beneath the syncer, durability
is the completion of the whole and
**MUST NOT** be inferred from the parts.

The uploader **MUST NOT** use multipart uploads. A block is at most the block
target plus one chunk `Max` ([§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), tens of MiB, and a single put carries that
comfortably. Multipart pays off when one object is large enough that a single
stream cannot fill the link. Here the pool already runs several puts at once, so
the link is filled across blocks instead of within one. Multipart would also add
a capability every backend must advertise, and a create–part–complete sequence
whose abandoned uploads leave parts that are billed and invisible to a listing.
Revisit if a measurement shows one put of a maximum-size block cannot saturate
the uplink even with the pool full.

**The put's length is the backend's problem, not the uploader's.** S3 needs an
object's length before its first byte, and compression makes the framed length
unknown until the last chunk is transformed. Framing and transforms happen below
the syncer ([RFC 8 §4.4](rfc-8-remote-tier.md#4.4%20The%20syncer%20MUST%20NOT%20observe%20that%20a%20transform%20happened)), so the S3 backend resolves it: it streams
the framed records into a local spool file and puts the file, whose length is
then known. The spool is on disk, so the memory bound of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound) holds; it costs
one local write and read per block, and disk of at most the pool size times the
largest block. Transforming twice — once to count, once to send — would avoid the
disk and double the transform CPU; the spool is chosen because the link, not
local disk, is what a block upload waits on. A backend that accepts a put of
unknown length needs no spool.

The spool is placed and budgeted, never left to find free space. It lives in a
directory the engine gives the backend at construction, on local storage the
engine accounts for, and its space — `upload_workers` times the largest block — is
set aside from the local capacity the engine hands the journals
([RFC 6](rfc-6-engine.md)), so a spool write is never the one that finds the disk full. The backend
computes the put's checksum as it writes the spool ([RFC 8 §5.10](rfc-8-remote-tier.md#5.10%20Transfer%20practice%20a%20backend%20owes%20its%20service)). A spool write
that still fails for lack of space is a **local** error: the backend reports it as
one, and it does not make the store unhealthy, since the store is not at fault.

### 3.5 The bytes are stable for the duration

The bytes a put transfers **MUST NOT** change while it runs. [§3.3](#3.3%20The%20journal%20keeps%20referenced%20bytes%20stable) supplies that
for a journal reference; an implementation that transfers from anywhere else
**MUST** supply it some other way. As a second line, `src` recomputes each
chunk's hash as it reads and fails the transfer on a mismatch
([RFC 6 §5.1](rfc-6-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)): a violation becomes a refused put, not a misnamed object.

The name is a function of the content ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), so content that changes mid
transfer produces an object whose bytes do not match its name, and every later
verification of that object fails.

## 4. The fetcher

### 4.1 One fetch, two consumers

A fetch of chunks held only remotely — some ranges of one block, or all of it —
produces verified chunks, one at a time, and yields them to its caller. The caller
is the engine, which uses them twice: to answer the read that caused the fetch,
and, where its fill policy says so, to fill the journal so the next read is local
([RFC 6 §6.2](rfc-6-engine.md#6.2%20The%20reply%20is%20served%20from%20the%20fetched%20bytes), [RFC 0 §2.3](rfc-0-data-lifecycle.md#2.3%20Operations)). The syncer imports no journal and fills nothing ([§1.4](#1.4%20It%20is%20testable%20on%20its%20own)).

Both uses **MUST** read the same bytes and **MUST NOT** modify them. Each chunk is
verified before it is yielded, which is what makes one buffer safe for both. A
reader waiting on one chunk is answered when that chunk arrives, not when the
whole range has.

![A fetch is verifiable exactly when the caller can name a hash for what it returns: a chunk-aligned range is checked against the chunk's hash, an arbitrary byte range has no hash anywhere and would be returned on trust](img/rfc3-partial-retrieval.svg)

### 4.2 The reply neither waits on the fill nor fails with it

Answering the read before, and regardless of, the fill is the engine's obligation
([RFC 6 §6.2](rfc-6-engine.md#6.2%20The%20reply%20is%20served%20from%20the%20fetched%20bytes)), because the engine does both. What it needs from the syncer is that each
chunk is yielded as soon as it is verified, and stays valid until the caller's
next iteration ([§1.3](#1.3%20Interface)). A fill that copies the chunk into journal-owned space in
that window, and drops its own failure, delays nothing and fails nothing.

A fill that fails means the chunk is not cached locally. The bytes are correct —
they were verified — so withholding them, or failing a client's read because a
cache write failed, converts a performance problem into an error.

### 4.3 Concurrent demand for one chunk is one fetch

When a fetch that will bring a chunk is already in flight, a second demand for
that chunk **MUST** join it rather than start another.

Without this, N readers arriving together on one cold chunk cost N transfers, N
times the per-transfer memory of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound), and N fills of identical bytes. The uploader needs
no equivalent rule, because a boxed block is work that exists once.

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
  latch ([§5](#5.%20What%20belongs%20elsewhere)).
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
and the free capacity ([RFC 6](rfc-6-engine.md)). Two kinds exist and they differ in more than scale:

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
obligation is the engine's ([RFC 6 §6.4](rfc-6-engine.md#6.4%20Speculation%20is%20planned%20here%2C%20and%20yields%20to%20demand%20and%20to%20writes)), because only the engine sees journal capacity.
The syncer's part is that a `Prefetch` can be abandoned at any point without
failing anything. An engine that runs pre-warm on a flow of its own ([§1.3](#1.3%20Interface)) also
keeps it from holding its share's flow at its cap while that share's reads wait.

## 5. What belongs elsewhere

| Concern | Owner |
| --- | --- |
| A block's name | [RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block) |
| Framing, transforms, verification, the error set | [RFC 8 §3](rfc-8-remote-tier.md#3.%20The%20stored%20object), [§4](rfc-8-remote-tier.md#4.%20The%20transform%20chain), [§5.2](rfc-8-remote-tier.md#5.2%20Errors%20are%20a%20closed%20set), [§6](rfc-8-remote-tier.md#6.%20Reads%20are%20verified%20at%20this%20boundary) |
| What a durable acknowledgement is, per backend | [RFC 8 §5.6](rfc-8-remote-tier.md#5.6%20What%20acknowledgement%20means%20is%20the%20backend%27s%20to%20declare) |
| Recording durability and residency | [RFC 4](rfc-4-block-metadata.md) |
| Keeping referenced bytes stable; filling fetched ones | [RFC 1](rfc-1-journal.md) |
| Deleting a remote block | [RFC 7](rfc-7-gc.md), calling [RFC 8](rfc-8-remote-tier.md) directly |
| What to box, when to flush, what to evict, what to read ahead or pre-warm | [RFC 6](rfc-6-engine.md) |
| Whether the system keeps accepting writes when the remote is unavailable | RFC 6 |
| A share's health, from its store's state and its own flush outcomes | [RFC 6 §8.1](rfc-6-engine.md#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20flush%20included) |

**Health is probed, never stored as a flag.** A store's health is what its probe
last said ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)). A share's health is the engine's, computed from the store's
state together with the outcomes of its own flushes ([RFC 6 §8.1](rfc-6-engine.md#8.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20flush%20included)).

Neither **MUST** be a "remote is down" flag that stops attempts until something
clears it. Once attempts stop, none can succeed, so nothing ever sees the remote
come back: a 5 min outage becomes permanent. A probe that keeps running
while the store is unhealthy is what makes it recover on its own.

![A latched flag suppresses the attempts that would observe the recovery, so it is never cleared; a probe that runs whether or not transfers do observes the recovery, and one success makes the store healthy again](img/rfc3-health-latch.svg)

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

S5, S6, S8, S9 and S10 are the ones whose violation loses data. S1, S2, S3 and S13
are the ones whose violation stops the system, or makes it look stopped; S14, S15,
S17 and S18 are the ones whose violation spreads one store's or one flow's
trouble to others, or makes it permanent.

## 7. Conformance

Conformance is every **MUST** holding. The checks below are evidence for the ones
that fail *silently*, and a check is validated by reverting the code and watching
it fail on its own assertion.

Every check in [§7.1](#7.1%20Group%20A%20%E2%80%94%20silent%20data%20loss) requires a backend that can lose, corrupt, delay and
half-complete. A backend that always succeeds asserts nothing about any of them.

### 7.1 Group A — silent data loss

| Requirement | Check |
| --- | --- |
| [§2.5](#2.5%20An%20unknown%20outcome%20is%20not%20a%20success) unknown is failure | Drop the response of a put the backend committed; assert failure is reported, then that the retry produces one object, not two. |
| [§2.6](#2.6%20Durability%20is%20observed%2C%20never%20inferred) no inference | Drive the S3 backend against a service that closes the connection after receiving the whole body and before responding; assert `Upload` returns an error. |
| [§3.4](#3.4%20One%20put%20per%20block) partial put | Interrupt a transfer; assert the name is not retrievable and was not reported durable. |
| [§3.3](#3.3%20The%20journal%20keeps%20referenced%20bytes%20stable), [§3.5](#3.5%20The%20bytes%20are%20stable%20for%20the%20duration) stability | Give `Upload` a `src` whose bytes change mid-stream; assert the put is refused and nothing is stored under the name. That the journal keeps offered bytes stable is [RFC 1](rfc-1-journal.md)'s check. |
| [§4.1](#4.1%20One%20fetch%2C%20two%20consumers) verification | Corrupt the fetched bytes; assert no byte of the chunk is yielded. Corrupt one range of a ranged read; assert a range mismatch, not a corrupt-object error. |
| [§4.2](#4.2%20The%20reply%20neither%20waits%20on%20the%20fill%20nor%20fails%20with%20it) yield before fill | Hold one caller mid-iteration; assert the chunk it holds is unchanged until its next iteration, and that each chunk is yielded before the next is read from the store. |

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
| [§5](#5.%20What%20belongs%20elsewhere) health recovers | Fail the probe until the store is unhealthy, then restore the backend; assert it turns healthy and transfers resume with no external action. |
| [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work) failure window | Pass the probe while failing every put with `503` for the window; assert the store turns unhealthy, then healthy on the next probe, and that puts are retried once per probe interval, not continuously. |
| [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports) throughput floor | Trickle a put below the floor; assert it fails within the interval. Stall a reader of a fetch instead; assert the transfer does not fail. |
| [§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch) joining | Leave the loop as the first of three joined callers; assert the others complete. Fail the fetch; assert all callers see the error and the next demand starts a fresh fetch. Join a speculative fetch with a demand; assert it is promoted. Join after the needed chunk has passed; assert a new fetch. Stop reading as one caller; assert it is detached within the bound and the others continue. |
| [§1.3](#1.3%20Interface) lifecycle | Call on a closed flow and after `Close`; assert both fail. Assert `Close` returns only when transfers have ended, and that `src` is never called after `Upload` returns. |
| [§1.3](#1.3%20Interface) stream rules | Assert an error is yielded once and ends the stream, and that leaving the loop stops the store's read. |
| [§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows) store level | Run eight flows on a slow store and one on a healthy one; assert the healthy store's flow starts within one round and the slow store never holds more than its cap. |

### 7.3 What must not stand in

- **An in-memory backend MUST NOT be the only one under test.** It cannot lose an
  acknowledged write, half-complete or delay, so it asserts nothing about [§2.5](#2.5%20An%20unknown%20outcome%20is%20not%20a%20success),
  [§2.6](#2.6%20Durability%20is%20observed%2C%20never%20inferred) or [§3.4](#3.4%20One%20put%20per%20block).
- **A backend that returns instantly MUST NOT be used for [§2](#2.%20What%20both%20halves%20obey) or [§4.4](#4.4%20Speculation%20does%20not%20delay%20demand).** With no
  latency the pool is never the binding constraint, so every check in those
  sections passes without exercising what it names.
- **A lifetime count MUST NOT be the source for a store's health.** Health is the
  latest probe, or a window of failures since it ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)); a since-start rate
  never lets a recovered store count as healthy.

### 7.4 Benchmarks

Conformance says the syncer is correct; these say what it costs. They use the
real clock — `synctest` makes time free, which is the opposite of what a benchmark
wants — and a fake store with a simulated link: a per-call latency and a
per-byte time, never zero ([§7.3](#7.3%20What%20must%20not%20stand%20in)). Each reports throughput, allocations and
peak memory through `b.ReportMetric`, and a result is recorded with the commit it
measured.

| # | Measures | Setup | Expect |
| --- | --- | --- | --- |
| B1 | the syncer's own overhead | the same pool size, once through the syncer and once as raw calls on the fake store | throughput within a few percent of raw; no allocation per chunk once warm, buffers being reused |
| B2 | scheduler cost | one transfer dispatched with 1, 10, 100 and 1,000 flows waiting | flat: DRR is O(1) per dispatch ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)) |
| B3 | fairness under load | one flow saturating uploads on a slow store; a second issuing fetches | the second flow's p99 wait within one round; the first never above its cap |
| B4 | joined fetches | N readers on one cold chunk | one transfer, whatever N ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) |
| B5 | the real link | the syncer over MinIO locally and over S3 in the benchmark environment, at the pool sizes the sizing tool printed ([§2.11](#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)) | within a few percent of the tool's raw figure; the gap is what the syncer costs on a real link |

Benchmarks run on `develop`, never on a pull request; what a pull request checks
instead is in [§8.4](#8.4%20What%20CI%20checks%20instead%20of%20timing). B5 needs a real store and credentials, and runs where those are.

A benchmark whose fake answers instantly measures the scheduler's lock, not the
syncer: the pool never fills, so nothing it exists to do happens.

B5 reports its result as a **fraction of the sizing tool's raw figure** for the
same store, and exports the pools' occupancy beside it. The external benchmark of
v0.33.0 measured about 28% of raw S3 through the whole stack; a syncer pool that
is not full while that happens means the limit is upstream of the syncer, and
[RFC 6 §13.5](rfc-6-engine.md#13.5%20Benchmarks) measures the pipeline end to end to find where.

## 8. Test plan and performance targets

[§7](#7.%20Conformance) says what must be checked and how a check is validated, and [§1.4](#1.4%20It%20is%20testable%20on%20its%20own) what
a check is built from. This section is the plan around it, in the shape
[RFC 1 §12](rfc-1-journal.md#12.%20Test%20plan%20and%20performance%20targets) set.

### 8.1 Kinds of test

| Kind | What it covers | How |
| --- | --- | --- |
| Conformance | every check in [§7](#7.%20Conformance), Groups A and B | the fault-injecting fake store of [§1.4](#1.4%20It%20is%20testable%20on%20its%20own), virtual time, race detector |
| Real backend | what the fake cannot fail like ([§7.3](#7.3%20What%20must%20not%20stand%20in)) | nightly: the Group A checks and the lifecycle checks against MinIO in a container, with a fault proxy between them |
| Scheduler model | S17 and S18 over arrivals no hand-written case thinks of | random flows, block sizes and arrival times under `synctest`, checked after every dispatch against a reference deficit-round-robin: who runs next, and that no flow or store exceeds its cap. A failing run is shrunk and kept |
| Memory | S2 | held-pool heap checks ([§1.4](#1.4%20It%20is%20testable%20on%20its%20own)) |
| Structure | the dependency set of [§1.4](#1.4%20It%20is%20testable%20on%20its%20own) | an import test |
| Benchmark | [§7.4](#7.4%20Benchmarks), B1–B5 | real time; B1–B4 on a fake with a simulated link, B5 on a real store; on `develop`, never on a pull request ([§8.4](#8.4%20What%20CI%20checks%20instead%20of%20timing)) |
| Soak | leaks and drift | hours, nightly, against MinIO with outages injected on a cycle, asserting goroutines, open connections, `inflight_bytes` and queue depth return to idle after each |
| Sizing tool | [§2.11](#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool) | the tool against a fake with a known knee; it **MUST** print that knee |

There is no crash test here. The syncer persists nothing (S7), so a crash leaves
nothing of its own to recover; what a crash does to transfers in flight is the
engine's to test ([RFC 6 §2.5](rfc-6-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)).

### 8.2 Edge cases

The conformance, model and fault tests **MUST** reach these.

**Shape of a transfer**

- a block of one chunk, and one at the largest block size;
- a `src` that yields an error before its first chunk, and after its last;
- `want` empty, one range, every range, adjacent ranges, ranges that are not
  adjacent, the same range twice, and a range ending at the object's last byte.

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

- a probe that never returns. It **MUST** be bounded like any call and count as a
  failure; a hung probe that left the store healthy would keep sending traffic to
  a dead store.

### 8.3 Faults and outside interference

Faults are injected systematically: the fake store fails the *n*th call of a
scripted run, for every *n*, with each failure kind of [§1.4](#1.4%20It%20is%20testable%20on%20its%20own) — transient,
terminal, committed with the response lost, stopped part-way, corrupted, held.
After each, every transfer has ended with a report (S4), nothing unconfirmed was
reported durable (S5, S6), and no chunk that failed verification was yielded
(S10).

Against the real backend, a fault proxy adds what the fake cannot: a connection
reset mid-body, a bandwidth cap below the throughput floor, `503 SlowDown`, and
a response dropped after the store committed.

The store can also change behind the syncer, and the tests cover what it then
sees:

| Change | What the syncer does |
| --- | --- |
| an object deleted | the `Get` fails as absent; the engine re-resolves once ([RFC 6 §6.7](rfc-6-engine.md#6.7%20An%20absent%20object%20is%20re-resolved%20exactly%20once)) |
| an object overwritten with other bytes | verification fails; reported as corrupt, or as a range mismatch where the ranges no longer line up |
| the bucket removed, or credentials revoked | terminal failures; the probe fails and the store turns unhealthy |
| the store slowed below the floor | transfers fail on the floor and are retried within the bound |

### 8.4 What CI checks instead of timing

Tiers are those of [RFC 1 §12.4](rfc-1-journal.md#12.4%20What%20CI%20checks%20instead%20of%20timing): on a pull request, only the counts below, under
`synctest` and against the fake, with the same answer on any runner. Each is the
count behind one benchmark:

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
recorded in absolute numbers ([§8.6](#8.6%20Recording%20results)).

The goal is to saturate the link. The syncer moves bytes it does not look at, so
anything it costs on top of the store is overhead to remove, not a budget to
spend.

| # | Metric | Proposed target |
| --- | --- | --- |
| B1 | throughput through the syncer, same pool size | ≥ 97% of raw calls on the fake |
| B2 | one dispatch with 1,000 flows waiting | ≤ 1 µs, and flat from 1 flow |
| B3 | a fetch's queue wait while another flow saturates uploads | p99 within one round |
| B5 | throughput through the syncer on a real store | ≥ 95% of the sizing tool's raw figure; below 90% is a regression. v0.33.0 managed 28% through the whole stack |
| — | peak memory per half | ≤ pool × the per-transfer figure of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound), plus 10% |
| — | a failed store refused | within one probe interval plus the probe's bound |
| — | a recovered store accepting work | within one probe interval |

B4 is a count, not a speed, and is fully checked in CI.

### 8.6 Recording results

Results are recorded as [RFC 1 §12.6](rfc-1-journal.md#12.6%20Recording%20results) requires, with the link in place of
the disk: the backend and its region, round-trip time, bandwidth, object size,
the pool sizes, and the sizing tool's raw figures for that store. For B1–B4 the
fake's simulated latency and per-byte time are part of the result.

## 9. Open questions

Numbering is stable: an answered question keeps its number so that references to
it stay valid.

1. **Pool sizes.** **Partly answered** by [§2.11](#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool). Both bounds are memory bounds with a known formula, so both can
   be set from a budget. What the right values are for a saturating SMB workload,
   and whether the halves contend enough on a shared link to need a joint cap
   rather than two independent ones, are unmeasured. The sizing tool answers the first ([§2.11](#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)); the second wants a third step that runs puts and gets together at their recommended sizes and compares the total against the sum of the two alone.
2. **Whether either pool should adapt.** **Decided: no** ([§2.11](#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)). The
   knee of a link is measured once by a tool and the pools are fixed from it. An
   adaptive pool would find the knee at runtime at the cost of a control loop, a
   second thing to observe, and a failure mode where the window collapses and is
   indistinguishable from a slow network. Reopen only on a measurement showing a
   fixed pool, sized by the tool, leaves throughput on the table.
3. **How pre-warm yields.** Moved to [RFC 6 §6.4](rfc-6-engine.md#6.4%20Speculation%20is%20planned%20here%2C%20and%20yields%20to%20demand%20and%20to%20writes), which now owns the obligation.
   What follows is kept for that discussion. The obligation is stated; the mechanism is not.
   A capacity reservation, a low-water mark, and cancellation on pressure are all
   plausible and they behave differently when a write burst arrives mid-warm.
4. **What durable acknowledgement is, per backend.** **Answered** in [§2.6](#2.6%20Durability%20is%20observed%2C%20never%20inferred).
5. **Where the retry bound lives.** **Answered** in [§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports): in the syncer
   only; the backend's client makes one attempt. Today's double retry is a
   deviation ([§10](#10.%20Deviations)).
6. **Deviation pass against this shape.** **Answered:** run against
   `pkg/block/engine` and `pkg/block/remote`; its findings are
   [§10](#10.%20Deviations) D7 to D20.
7. **The put's length when the upload streams.** **Answered** in [§3.4](#3.4%20One%20put%20per%20block): the
   backend spools to a local file.
8. **One syncer per share today.** Moved to [§10](#10.%20Deviations).
9. **Probe interval and flapping** ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)). One failed probe makes a store unhealthy
   and one success makes it healthy. Whether a store that fails intermittently needs a run of failures
   before turning unhealthy depends on how often real probes fail spuriously, which is
   unmeasured.
10. **The fixed values** ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)). The per-flow cap, queue length, probe interval
    and the two pool defaults are reasoned, not measured. The cap trades a flow
    alone on the system (wants it high) against the wait of a second flow's
    first transfer when the first flow's store is slow (wants it low).
11. **Today's settings differ.** Moved to [§10](#10.%20Deviations).

12. **Considered and deferred.** Each is in use elsewhere and each waits for a
    measurement that asks for it: hedged cold reads ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)); a bandwidth
    cap per direction, as rclone's `--bwlimit` and JuiceFS's
    `--upload-limit` / `--download-limit`, which [§2.1](#2.1%20A%20worker%20pool%20is%20the%20only%20concurrency%20control) permits only with its
    interaction with the pool stated; spreading connections across the service's
    addresses ([RFC 8 §5.10](rfc-8-remote-tier.md#5.10%20Transfer%20practice%20a%20backend%20owes%20its%20service)).

## 10. Deviations

Where today's code departs from this document. Each is a change to make, not a
question to answer.

| # | Rule | Today | Where |
| --- | --- | --- | --- |
| D1 | one syncer per process ([§1.3](#1.3%20Interface)) | each share's engine owns its own syncer; only the backend client beneath is shared between shares on one store | `runtime/shares/blockstore_config.go` |
| D2 | two process-wide settings ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) | the upload window is set per remote store (`parallel_uploads`), adaptive between 16 and 64 by default, up to 256 pinned; the adaptive controller and its bounds are deleted by [§2.11](#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool) | `engine/types.go:24`–`:44`; `runtime/blockstore_init.go:99` |
| D3 | `fetch_workers` a setting, default 32 ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) | deduced as max(8, 2 × CPUs) and cannot be set | `cmd/dfs/commands/start.go:366`; `pkg/config/init.go:163` |
| D4 | one probe interval, one failure turns unhealthy ([§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work)) | 30 s healthy, 5 s unhealthy, three failures to turn unhealthy, and a separate demand-fetch timeout | `engine/types.go:108`–`:111`, `:130`–`:133` |
| D5 | the syncer is the only layer that retries ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) | the S3 SDK retries up to `maxAttempts` with backoff to 30 s, 429 included, beneath the syncer's own retries | `remote/s3/store.go:162`–`:170` |
| D6 | a backend's client limit derived from the pools ([RFC 8 §5.9](rfc-8-remote-tier.md#5.9%20A%20backend%27s%20client%20never%20queues%20below%20the%20pools)) | fixed at 256 connections | `remote/s3/store.go:49` |
| D7 | the uploader reads a journal reference and streams ([§3.2](#3.2%20It%20holds%20a%20reference%2C%20not%20a%20copy), [§1.3](#1.3%20Interface)) | the carver copies journal bytes into a block buffer before an upload slot is free, and the put sends the whole sealed block from a second in-memory buffer; per-transfer memory is two block-sized buffers and is stated nowhere. The fix is decided: a block plan read through the journal's offered reader ([RFC 6 §5.1](rfc-6-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output), [RFC 1 §3.3](rfc-1-journal.md#3.3%20Flush)) | `engine/flush_closure.go:112`–`:151`, `:425`; `engine/flush.go:397`–`:446` |
| D8 | a failed fill does not fail the read ([§4.2](#4.2%20The%20reply%20neither%20waits%20on%20the%20fill%20nor%20fails%20with%20it), S11) | the read is answered by re-reading the journal after the fill; a fill error fails the fetching caller and every joined one | `engine/fetch.go:669`–`:673`; `engine/read_internal.go:117`–`:131` |
| D9 | a retry after an unknown outcome writes the same object ([§2.5](#2.5%20An%20unknown%20outcome%20is%20not%20a%20success)) | block names are 16 random bytes, so a retry writes a second object and the first is an orphan for GC; S5 still holds. Recorded as [RFC 2 §4.2.1](rfc-2-carver.md#4.2.1%20Deviation%20%E2%80%94%20a%20block%27s%20identity%20is%20generated%2C%20in%20two%20places) | `engine/flush.go:392`; `block/block_record.go:29`–`:35` |
| D10 | one pool bounds fetches in flight ([§2.1](#2.1%20A%20worker%20pool%20is%20the%20only%20concurrency%20control), S1) | each demand read and each warm run builds its own fetch group of the configured size, beside the prefetch queue's own workers; fetches in flight, and their memory, grow with concurrent readers | `engine/fetch.go:31`–`:39`, `:540`–`:549`; `engine/warm.go:175`; `engine/sync_queue.go:89`–`:92` |
| D11 | one caller leaving does not cancel a joined fetch ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) | the shared fetch runs on the first caller's context, so its timeout or cancellation fails every caller that joined | `engine/fetch.go:537`, `:598`–`:608`, `:653` |
| D12 | a fetch is joined per chunk, not per byte offset ([§4.3](#4.3%20Concurrent%20demand%20for%20one%20chunk%20is%20one%20fetch)) | fetches are ranged reads of one chunk, which [§1.3](#1.3%20Interface) permits, but they are joined by chunk *and starting offset*, so two reads of one chunk from different offsets fetch it twice | `engine/fetch.go:340`, `:592`–`:595` |
| D13 | pre-warm never drives the journal to refusing writes ([§4.5](#4.5%20Speculation%20is%20executed%20here%20and%20decided%20elsewhere)) | a warm run fills until the journal reports full, then stops | `engine/warm.go:175`–`:196` |
| D14 | an unhealthy store is probed at once on a transient failure, logs as [§2.8](#2.8%20An%20unhealthy%20store%20refuses%20work) says, and its refusals name it | probing is on the ticker only, so a dead store takes traffic for about three intervals; the unhealthy line is a warning carrying a failure count and no store name or error; there is no periodic line; a refused call's error names neither the store nor the probe's error | `engine/sync_health.go:141`–`:181`, `:255`–`:258` |
| D15 | everything but the two pool sizes is fixed and derived ([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) | further hard-coded values: a prefetch queue of 1,000, a 5 min prefetch timeout, a 60 s demand timeout ([§2.4](#2.4%20Every%20transfer%20terminates%2C%20and%20reports) replaces the last with a throughput floor) | `engine/sync_queue.go:49`–`:51`, `:186`; `engine/types.go:52` |
| D16 | `Close` returns once every transfer has ended ([§1.3](#1.3%20Interface)) | each wait gives up after 30 s and returns anyway; demand fetches on reader goroutines are not tracked | `engine/sync_lifecycle.go:187`–`:203`; `engine/sync_queue.go:118`–`:123` |
| D17 | the syncer decides nothing and persists nothing ([§1.1](#1.1%20Non-goals), [§2.7](#2.7%20It%20reports%3B%20it%20does%20not%20persist)) | the upload side decides when to carve and commits block records itself; the component this document describes does not exist as a boundary in code yet | `engine/carve_dispatch.go:37`–`:48`; `engine/flush.go:468` |
| D18 | every store is durable; none is declared otherwise ([§2.6](#2.6%20Durability%20is%20observed%2C%20never%20inferred)) | a store's `durable` setting can declare a store not durable, the in-memory backend defaults to not durable, and the commit rule branches on it; the setting and the branch are to be removed | `runtime/shares/blockstore_config.go:856`–`:860`; `remote/memory/store.go:228`; `remote/s3/store.go:126`–`:134` |
| D19 | a put carries an end-to-end checksum ([RFC 8 §5.10](rfc-8-remote-tier.md#5.10%20Transfer%20practice%20a%20backend%20owes%20its%20service)) | the S3 client disables the SDK's default request checksums and response validation | `remote/s3/store.go:185`–`:192` |
| D20 | fair scheduling across stores and flows ([§2.9](#2.9%20Workers%20are%20shared%20fairly%20across%20flows)) | no per-flow or per-store queue, round robin or cap exists; demand fetches run on the reader's goroutine and uploads share a per-share window | `engine/fetch.go`; `engine/upload_window.go` |

Checked and satisfied: no multipart upload anywhere ([§3.4](#3.4%20One%20put%20per%20block)); an unknown outcome is
reported as a failure before any commit (S5); durability is reported only after
the put and its commit, in order (S6); every fetched chunk is hash-verified before
it is filled or served (S10); probing continues while unhealthy and one success
recovers (S15, [§5](#5.%20What%20belongs%20elsewhere)); a failed shared fetch is not remembered; demand fetches never
queue behind prefetch (S13); health gates run before any backend call (S14); the
prefetch queue is bounded and drops when full ([§2.3](#2.3%20Backpressure%20propagates%3B%20it%20does%20not%20buffer)); a fill that raced a write is
fenced off (S8).

## Appendix A. Running the pool-sizing tool

Descriptive, not normative, except where a rule of [§2.11](#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool) is restated. The
numbers below are starting points for the tool's own defaults and are unmeasured.

### A.1 Setup

1. Build the store from the same configuration a share uses, through the same
   constructor the server calls, with its client sized to the highest step
   ([RFC 8 §5.9](rfc-8-remote-tier.md#5.9%20A%20backend%27s%20client%20never%20queues%20below%20the%20pools)).
2. Generate one buffer of random bytes the size of the largest block, block
   target plus chunk `Max`. Random bytes do not compress, so a backend or proxy
   that compresses in transit cannot flatter the result.
3. Choose a scratch prefix, `_speedtest/<random id>/`, that no share uses, and
   record every key written under it from the first put onward.
4. Print the data the run will move and stop if it exceeds `--max-bytes`. On a
   metered service a run moves tens of GiB, and reads out of a cloud provider are
   charged per GiB.

### A.2 One phase per direction

Puts first, then gets over the objects the put phase wrote. Each phase steps
through concurrency 1, 2, 4, 8, … and at each step:

- `n` goroutines loop the operation, each put to a fresh key;
- the step runs for a fixed time, 15 s by default, rather than a fixed count, so
  a slow link and a fast one spend the same time per step;
- the first 3 s are discarded: connections are still opening and TCP is still
  ramping, and counting them understates every low step;
- the step records throughput in MiB/s, the p50 and p99 latency per call, and the
  time to first byte separately from the transfer time, since a store can be slow
  to answer and fast to send, or the reverse;
- object sizes step through 64 KiB, 1 MiB, the chunk target and the largest block.
  The external benchmark of v0.33.0 found single-connection gets 10× slower on one
  store than another at 100 MB and much less apart at 10 MB: per-request latency,
  not bandwidth, and only small objects show it;
- single-connection cells are repeated at least three times, and the run records
  the round-trip time and hop count to the store.

Latency is what explains the result. While throughput doubles with `n` and
latency stays flat, the link is not full. Where latency climbs and throughput
flattens, requests are queueing at the link or at the service, and more workers
buy only memory.

Where the service limits request rate per key prefix — S3 does, at a few
thousand puts per second per prefix — the tool spreads keys over several
sub-prefixes so it measures the link rather than the service's throttle. With
blocks tens of MiB in size this matters only on very fast links.

### A.3 Stopping rule

A phase stops at the first step `n` where doubling returned less than 10%:
`throughput(2n) < 1.10 × throughput(n)`. That `n` is the knee. A phase also stops
when throughput falls between two steps, and at `--max-concurrency`.

### A.4 Output

```
store s3-main   block 20 MiB

upload   n   MiB/s   p50     p99
         1     95    210ms   260ms
         2    188    212ms   270ms
         4    370    215ms   290ms
         8    710    224ms   330ms
        16   1090    290ms   520ms
        32   1150    550ms   1.1s    ← knee 16: +5% for twice the workers
fetch    …

recommend  upload_workers = 22   (knee 16 ÷ 0.75)
           fetch_workers  = 43   (knee 32 ÷ 0.75)
memory     upload ≈ 440 MiB, fetch ≈ 860 MiB   (pool × largest block)
note       measured from this host at this time; transforms not included
```

The recommendation is the knee divided by the per-flow cap and rounded up
([§2.10](#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)), capped by `--memory-budget` when given. With several stores,
each is measured and the largest recommendation per direction is printed, since
one pool serves them all. The memory line uses the largest block, the upper
figure of [§2.2](#2.2%20The%20pool%20size%20is%20a%20memory%20bound), because the tool cannot know whether a backend's client buffers
the whole object.

### A.5 Cleanup

Delete every recorded key when the run ends, on success, error or interrupt. A
run killed before it could clean up leaves its key list in a local file, and the
next run's first act is to delete what that list names.
