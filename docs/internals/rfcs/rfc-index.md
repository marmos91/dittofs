---
tags:
  - rfc-index
aliases:
  - RFCs
  - DittoFS RFCs
---

# DittoFS RFCs

The specification of DittoFS, from the bytes a client writes to the objects in the remote tier,
and the layers around them. [RFC 0](rfc-0-data-lifecycle.md) is the root: it defines the terms,
the residency function and the invariants every other RFC inherits. Start there, and read in
order: each part builds on the ones before it.

| RFC | Component | Owns |
| --- | --- | --- |
| **Foundations** | | |
| [RFC 0](rfc-0-data-lifecycle.md) | data lifecycle | terms, data model, residency, invariants, failure model |
| **Content**, in write-path order | | |
| [RFC 1](rfc-1-journal.md) | journal | local bytes, one journal per device: on-disk format, placement, crash safety, capacity |
| [RFC 2](rfc-2-carver.md) | carver | bytes → chunks → blocks: boundaries, identity, packing |
| [RFC 3](rfc-3-syncer.md) | syncer | transferring blocks to and from the remote tier |
| [RFC 4](rfc-4-remote-tier.md) | remote tier | object format and backend contract |
| [RFC 5](rfc-5-transforms.md) | transforms | compression, encryption, threat model |
| **Metadata** | | |
| [RFC 6](rfc-6-block-metadata.md) | block metadata | shape, holes, refs, chunks, blocks, refcounts |
| [RFC 7](rfc-7-namespace-metadata.md) | namespace metadata | files, directories, handles, permissions, locks |
| **Composition** | | |
| [RFC 8](rfc-8-engine.md) | engine | composition, policy, the facade adapters call |
| [RFC 9](rfc-9-gc.md) | GC | sweep, relocation, remote deletion |
| **Cluster** | | |
| [RFC 10](rfc-10-journal-replication.md) | journal replication | *draft* — owner-driven replication, fencing, seal, catch-up, reads from replicas |
| [RFC 11](rfc-11-ownership.md) | ownership | *draft* — ownership units (a share by default), leases, handover, failover, forwarding |
| **Data management** | | |
| [RFC 12](rfc-12-snapshots.md) | snapshots, backups and share migration | *draft* — snapshots (read-only, writable clones, scheduled with retention), metadata backup, restore to a new share, moving a share between installations on one bucket |
| **Security** | | |
| RFC 13 | identity and authentication | *planned* — principals, authentication flavours, identity mapping, squashing |
| RFC 14 | authorization | *planned* — one abstract ACL model, its protocol mappings, evaluation |
| **Protocols** | | |
| RFC 15 | adapter model | *planned* — the protocol handler contract, auth context, error mapping, dispatch |
| RFC 16 | NFS | *planned* — decisions the NFS standards leave open |
| RFC 17 | SMB | *planned* — decisions the SMB standards leave open |
| **Operations** | | |
| RFC 18 | control plane | *planned* — runtime, share lifecycle, configuration, management API |
| RFC 19 | resources and concurrency | *planned* — memory budgets, buffer pools, admission, backpressure |
| RFC 20 | observability | *planned* — metric and label conventions, health derivation, events |

[The block data-flow split](rfc-block-dataflow.md) is the earlier plan the storage RFCs grew out of.

## Dependencies

An arrow reads "builds on". Every RFC builds on RFC 0; those edges are left out.

```mermaid
graph LR
  R1[1 journal]
  R2[2 carver] --> R1
  R3[3 syncer] --> R1 & R2 & R4
  R4[4 remote tier] --> R3
  R5[5 transforms] --> R2 & R4
  R6[6 block metadata] --> R2 & R3
  R7[7 namespace metadata] --> R6
  R8[8 engine] --> R1 & R2 & R3 & R6 & R7 & R9 & R4
  R9[9 GC] --> R2 & R6 & R4
```

## Conventions

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** in
every RFC of this set are to be interpreted as in RFC 2119.

- Each RFC's frontmatter carries its status and what it builds on
  (`depends_on`); the body does not repeat them.
- Normative text names no product, package, file or function. Products appear
  only in appendices labelled as a profile, an example, prior art, a measurement
  or where the current code differs.
- Signatures are indicative; the obligations around them are normative.
- `ponytail:` notes mark a deliberately simple design and name what would justify
  replacing it; `decision:` notes mark a deliberately narrow rule and name what
  would overturn it.

## Test tiers

Every RFC's test plan follows the rules here; an RFC states only what is
specific to its component.

**What conformance is.** An implementation conforms when every **MUST** in its
RFC holds. Each RFC's named checks are not that definition: they are evidence
for the requirements that fail *silently*, where nothing errors and no test goes
red by accident. Passing them is necessary, not sufficient.

**How a check is validated.**

- Test at the consumer of a value, not its producer: a test beside the producer
  passes while the value is dropped downstream.
- Revert the code and watch the check fail on its own assertion. A check that has
  never failed is unverified; a build error is not a failure.
- Where the defect and the correct behaviour look alike at the point they happen
  (zeros instead of a fetch, a bit set too early), assert on the observation
  that tells them apart.

**What must not stand in.**

- A substitute that cannot exhibit a failure is not evidence of its absence: a
  sink whose writes always succeed, or an in-memory backend, is never the only
  backend under test for a check about durability, cost or crashes.
- The remote tier under test is a local emulator of the remote service, and at
  least one real service in the daily tier.
- A harness that constructs a component differently from production is never the
  only path under test.
- Records built by hand are never the only input to a component whose input
  another component produces in production. A hand-built fixture encodes its
  author's model of the producer; the leak that exists only for repeated or
  all-zero content, or only when two producers race, passes every such check.
  Fixtures include repeated and all-zero content.
- A cache between the check and the thing checked is disabled or bypassed, and
  the check asserts that the thing was reached: a "cold" read served from a
  client's page cache proves nothing about the remote fetch it was written for.

**A check that did not run is a failure.** Every tier knows how many checks it
was meant to run and fails when it ran fewer: a filter that matches nothing, a
package no job includes, a check gated on an environment variable nothing sets,
or a skip because a service was absent all report green otherwise. A skip is
reported with its reason, and a check skipped in every tier is deleted or moved
to a tier that runs it.

> [!important] Pending review — fixtures, caches and checks that never run
> Added after an external audit found suites that had not run in CI for months
> (an unset gate, a package missing from the derived list), a wrapper that
> reported success for a filter matching no test, cold-read tests that passed only
> when the client cache missed, and every GC test built from hand-made records —
> which is why a leak on repeated content went unseen.

**When tests run.**

| Tier | Runs | Contains |
| --- | --- | --- |
| **Per change** | on every proposed change | unit, conformance, property and fuzz seeds, model-based tests at a fixed budget, and each RFC's counted checks; minutes, no timing, no external service |
| **After merge** | after each merge to the integration branch | benchmarks on the reference box, recorded; a result more than 10% worse than the last is reported, not blocking |
| **Daily** | once a day on the integration branch | benchmarks that need space or hours, soaks, longer fuzz and model runs, and tests against real backends |

No timed check runs per change: a shared runner's device changes between runs,
so a timing gate either fails on noise or is set so wide it misses the
regressions that matter. Each RFC states the regressions it catches by counting
instead.

**Recording results.** Every benchmark result is recorded in absolute numbers,
with the commit, the box (CPU, memory, device and the device's own raw figures
for the run), the operating system and filesystem with its mount options, and
each row's concurrency. One named **reference box** carries the series over
time, so a change between two commits reads as a change in the code and not in
the hardware; results from other boxes are compared only with themselves. A
target is stated against something measured on the same box (the filesystem,
the hash, the link), so it holds on any hardware.

## Reading them in Obsidian

- Every `RFC N §x.y` is a link to that section. Hover for a preview; the backlinks pane shows
  every place that cites the section you are reading.
- Each RFC carries properties (`rfc`, `component`, `status`, `depends_on`), and
  [rfcs.base](rfcs.base) tabulates them.
- The Google Docs copies are generated from these files; comment in either place.
