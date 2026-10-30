---
rfc: 25
title: "RFC 25 — observability"
component: observability
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-3-syncer]]"
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
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-18-identity]]"
  - "[[rfc-19-authorization]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-21-nfs]]"
  - "[[rfc-22-smb]]"
  - "[[rfc-23-control-plane]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-26-catalog-backups]]"
  - "[[rfc-27-namespace-migration]]"
  - "[[rfc-29-test-harness]]"
  - "[[rfc-30-kubernetes]]"
aliases:
  - RFC 25
  - observability
tags:
  - rfc
---
# RFC 25 — observability

**Status:** draft. [§13](#13.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone adding a metric, a health condition, a log line or a span to
any component; anyone wiring DittoFS into monitoring, alerting or an audit
pipeline; anyone deciding whether a node is alive, ready or in trouble.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix B](#Appendix%20B%20%E2%80%94%20where%20the%20current%20code%20differs) says where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

Every RFC in this set ends with an Observability section: the metrics its
component exports, the health conditions it raises, what it logs. This RFC does
not repeat those tables. It sets the rules they all follow, so that the thirty
tables read as one system:

- how a metric is named, what unit it carries, which labels it may have and how
  many series it may cost;
- how the health conditions every component raises combine into the health of a
  share, a node and the installation, and what liveness and readiness mean;
- how the access-audit and quota events the filesystem service emits
  ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)), and the logon events the
  authenticator emits, are queued, stored, kept and
  handed to whoever consumes them;
- what a log line looks like, what it must never contain, and how a trace follows
  one request across nodes;
- which conditions should wake someone up.

**The problem, with one example.** At 02:00 the bucket behind the `profiles`
share stops accepting puts: someone tightened its policy. Every component does
its part. The syncer turns the store put-unhealthy with cause `ErrDenied`
([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)). The
engine stops offloading and, because no wait will fix a denial, answers writes
waiting on it as no space ([RFC 8 §11.1](rfc-8-engine.md#11.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included)).
Each exports a metric and writes one log line.

Without a rule that combines them, three things go wrong:

1. The node's readiness probe asks "is everything healthy?", gets no, and the
   orchestrator restarts the node. The restart fixes nothing — the bucket still
   refuses — but every NFS client now waits out a grace period and every SMB
   client reconnects.
2. Nobody is paged, because no single metric crossed a threshold anyone wrote an
   alert for; or everybody is paged, once per metric that moved.
3. An operator, chasing which share is hurt, adds a share label to the operation
   latency histogram. At 10⁴ shares that is about 10⁸ new series, and the
   monitoring system falls over at the moment it is needed.

With this RFC:

1. The syncer's state is a **condition** of the store, `store_put_unhealthy`,
   with its cause. Its severity follows from the cause: a denial makes writes fail,
   so it is **unhealthy**, not merely degraded (§3.2).
2. Every share on that store inherits the condition, so `profiles` reports
   unhealthy and names the store and the cause (§3.3). The node reports how many
   of its shares are unhealthy; it stays **live** and **ready**, since restarting
   it or sending its clients elsewhere would not help (§3.5, §3.6).
3. The registry marks the condition page-worthy after 5 minutes (§3.9). One page
   goes out, naming `profiles`, the store and the cause.
4. The operator finds the hurt share from the health report and from a per-share
   gauge exported for the worst shares only (§2.5), not from a new label.

Outcome: one page, no restart, and a monitoring system that is still standing.

![Each component raises named conditions on a subject; a store's or namespace's conditions flow to every share using it; share, node and installation statuses are the worst severity in scope; liveness reads only the watchdog, readiness only whether the node can serve, and alerts read the registry's page and ticket classes](img/rfc25-health.svg)

**The words you need.**

- **condition** — a named, current fact about a subject that makes it less than
  healthy: `store_put_unhealthy` on store `s3-eu`, cause access denied, since
  02:00:14 (§3.1).
- **subject** and **scope** — what a condition is about: a store, a namespace, a
  journal, a share, a node or the installation (§3.3).
- **severity** — **degraded** (everything still answered as specified, but
  headroom, redundancy or timeliness is reduced) or **unhealthy** (some class of
  operation is refused or failing now) (§3.2).
- **status** — the worst severity of the conditions in scope; healthy when there
  are none.
- **liveness** — whether the process is making progress at all; failing it asks
  to be restarted (§3.5).
- **readiness** — whether the node should be sent new clients (§3.6).
- **page**, **ticket** — the two alert classes: a person now, or a person soon
  (§7).
- **event stream** — an ordered record of decisions for a consumer outside the
  node: access, quota, logon and administrative events (§4).
- **spool** — the node-local, bounded, append-only files a stream is written to
  before it is exported (§4.3).
- **gap record** — a record a stream writes in place of events it had to drop, so
  a consumer knows the stream is incomplete and where (§4.4).
- **entity label** — a label whose values are configured objects (a store, a
  namespace, a journal, a backup location), allowed but capped (§2.4).
- **worst-n gauge** — a per-share gauge exported only for the shares worst by
  that gauge (§2.5).

**What this RFC promises.**

- Every metric in the set has a name that says its owner, its unit and its type,
  and a label set whose size is known before deployment.
- A node at the customer scale — 10⁴ shares, 10⁵ users — exports a bounded number
  of series, whatever its load, and a test counts them.
- Health is derived from conditions, never declared, and never latched: a
  condition clears when its cause does.
- A dependency failing never fails liveness, and readiness fails only when
  sending the node's clients elsewhere would help.
- Every condition has one severity rule and one alert class, in one registry.
- Audit and quota events never slow an operation; when one is lost, the stream
  says so, in place.
- Audit data is readable only by those allowed to audit; it never appears in a
  log or a metric.
- No log line, span or metric carries a secret or file content; file names
  appear only in the debug level, which turns itself off when turned on at run
  time and raises a condition when set at start.

**How the rest is organised.** §1 is purpose and boundaries. §2 is the metric
conventions, §3 health derivation, §4 the event streams, §5 logs, §6 tracing and
§7 alerting. §8 is the components that carry them and their lifecycle, §9 the
invariants, §10 this RFC's own metrics, and §11 the audit of every other RFC's
table against §2, as edits. §12 is conformance and §13 open
questions. Appendix A is prior art; Appendix B where the current code differs.

### Scenarios

**bob is refused an open on `profiles/alice`, and the audit consumer is slow.**

1. The share's audit policy selects denials. bob-pc's `CREATE` reaches the
   chokepoint, which refuses it; the service emits an `AccessEvent` with
   `Allowed` false ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks),
   [RFC 19 §3.6](rfc-19-authorization.md#3.6%20Audit%20entries%20select%20events)).
2. The event goes into the access stream's in-memory ring and the call returns at
   once (§4.3). The writer appends it to the spool within a second.
3. The external shipper that tails the spool has been down for a day. The spool
   reaches its byte bound; the oldest file is deleted. The node raises
   `event_gap` and writes a gap record naming the range of sequence
   numbers lost (§4.4, §4.6).
4. An account holding the right to read the audit reads bob's refusal through
   the management API; a `viewer` cannot (§4.8).

Outcome: the refusal is recorded, the operation never waited, and the loss of
older records is itself recorded.

**An operator wants the latency of `builds` alone.**

1. `vfs_op_seconds` has no share label, and gets none (§2.4).
2. The share's figures that are gauges — dirty bytes, oldest unoffloaded age,
   offload backlog — are exported for `builds` whenever it is among the worst 20
   shares by that figure, and are always readable through the management API
   (§2.5).
3. For latency, the operator samples traces: each span carries the share ID as an
   attribute, and a trace backend can filter on it (§6.1).

Outcome: the question is answered, and no metric grows by the number of shares.

A restart in grace is §3.6's table, row "in grace"; a trace across nodes is §6.2.

## 1. Purpose

A component's metrics, conditions and log lines are written by whoever writes the
component, one RFC at a time. Without shared rules they grow apart: one table
counts in milliseconds and another in seconds, one labels by share and melts the
monitoring system, two tables count the same event under two names, and a
readiness probe built on "all components healthy" restarts a node for a fault the
restart cannot fix. This RFC fixes the rules once, and owns the parts no
component can own: how health combines, and how the event streams leave the node.

### 1.1 Non-goals

DittoFS exports; it does not store metric history, route alerts, archive audit records
or profile ([RFC 16 §7.3](rfc-16-metadata-store.md#7.3%20Profiling)). Everything else
not specified here has its owner in §1.2.

### 1.2 Who owns what

| Topic | Owner |
| --- | --- |
| a component's metrics, conditions, log lines | that component's RFC, following §2–§5 |
| metric naming, units, types, labels, cardinality, buckets | this RFC, §2 |
| the condition registry: names, scope, severity, alert class | this RFC, §3.9; each new condition adds a row |
| how conditions combine into share, node and installation status | this RFC, §3.3 |
| what liveness, readiness and health answers report | this RFC, §3.5–§3.7 |
| the endpoints that serve them, and who may call them | [RFC 23](rfc-23-control-plane.md) |
| the Kubernetes probes and the cluster resource's conditions | [RFC 30 §4.3](rfc-30-kubernetes.md#4.3%20Health), reading §3.5–§3.6 |
| where access and quota events are emitted, and their fields | [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) |
| which operations produce an access event: the share's audit policy | [RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings), applied by [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) |
| where logon events are emitted, and logon throttling | [RFC 18 §3.1](rfc-18-identity.md#3.1%20The%20authentication%20interface) |
| which decisions an audit entry selects | [RFC 19 §3.6](rfc-19-authorization.md#3.6%20Audit%20entries%20select%20events) |
| delivery, bounds, drops, retention, format and access of the streams | this RFC, §4 |
| the management audit records, their content, durability and retention | [RFC 23 §6](rfc-23-control-plane.md#6.%20The%20audit) |
| the roles that may read health detail and the audit | [RFC 23 §5.2](rfc-23-control-plane.md#5.2%20Roles) |
| that no secret leaves a record | [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets); this RFC extends it to logs, spans and events (§5.4) |
| the memory the event rings take | this RFC, §4.4; [RFC 24 §5.1](rfc-24-resources.md#5.1%20No%20queue%20is%20unbounded) lists the queue |

## 2. Metrics

### 2.1 Names

A metric name is `<component>_<what>[_<unit>][_total]`, lower case, words
joined by underscores.

- **The exporter adds one fixed prefix, `dittofs_`, to every name.** Every RFC
  shows names without it. Installations and nodes are told apart by the labels the
  scraper attaches to the target, never by a prefix, so one dashboard serves every
  installation.
- **`<component>` names the owning RFC**, from the registry below. A prefix has
  exactly one owner; an RFC may own several. A metric whose prefix belongs to
  another RFC is a defect in the RFC that uses it.
- **`<what>` is a noun phrase for the thing measured**, not the code that measures
  it: `queue_depth`, not `pending_len`.
- A name **MUST NOT** change once exported, except with its owning RFC's change
  and a note in the release, since every dashboard and alert keys on it.

| Prefix | Owner |
| --- | --- |
| `journal_` | [RFC 1](rfc-1-journal.md) |
| `carver_` | [RFC 2](rfc-2-carver.md) |
| `syncer_` | [RFC 3](rfc-3-syncer.md) |
| `remote_`, `codec_` | [RFC 4](rfc-4-remote-tier.md) |
| `transform_`, `encryption_` | [RFC 5](rfc-5-transforms.md) |
| `blockmeta_` | [RFC 6](rfc-6-block-metadata.md) |
| `namespace_` | [RFC 7](rfc-7-namespace-metadata.md) |
| `engine_` | [RFC 8](rfc-8-engine.md) |
| `gc_` | [RFC 9](rfc-9-gc.md) |
| `replication_` | [RFC 10](rfc-10-journal-replication.md) |
| `shard_` | [RFC 11](rfc-11-ownership.md) |
| `snapshot_` | [RFC 12](rfc-12-snapshots.md) |
| `config_`, `format_` | [RFC 13](rfc-13-configuration.md) |
| `openstate_` | [RFC 14](rfc-14-open-state.md) |
| `topology_` | [RFC 15](rfc-15-topology.md) |
| `metadata_` | [RFC 16](rfc-16-metadata-store.md) |
| `vfs_` | [RFC 17](rfc-17-vfs.md) |
| `identity_` | [RFC 18](rfc-18-identity.md) |
| `acl_` | [RFC 19](rfc-19-authorization.md) |
| `adapter_` | [RFC 20](rfc-20-adapters.md) |
| `nfs_` | [RFC 21](rfc-21-nfs.md) |
| `smb_` | [RFC 22](rfc-22-smb.md) |
| `api_`, `node_`, `audit_` | [RFC 23](rfc-23-control-plane.md#11.%20Observability) |
| `memory_`, `cpu_`, `fd_` | [RFC 24](rfc-24-resources.md) |
| `health_`, `events_`, `build_` | this RFC (§10) |
| `backup_`, `export_`, `import_` | [RFC 26](rfc-26-catalog-backups.md) |
| `claim_`, `move_`, `rehome_` | [RFC 27](rfc-27-namespace-migration.md) |

A new component adds its row here with its first metric. The standard process
and language-runtime collectors keep their own conventional names, unprefixed:
they are the one exception, so that stock dashboards read them.

### 2.2 Units

- **Time is in seconds, sizes in bytes, fractions as ratios from 0 to 1.** Never
  milliseconds, kibibytes or percent. A float carries the precision.
- The unit is the last word before `_total`, and is spelled `seconds`, `bytes` or
  `ratio`. A rate is `_bytes_per_second`. A point in time is
  `_timestamp_seconds`, seconds since the Unix epoch. An age is `_age_seconds` or
  `_oldest_seconds`, measured at export.
- **A count of things has no unit word**: `syncer_workers`, `gc_trash_blocks`.
- **One metric has one unit.** A label **MUST NOT** select between units: a
  histogram of entries and bytes is two histograms.
- A value that bounds a measured metric — a setting's current value or a derived
  ceiling — is exported under that metric's name with `_limit`, so the two plot on
  one axis.

### 2.3 Types

| Type | For | Never for |
| --- | --- | --- |
| **counter** | events that happened: calls, bytes moved, refusals, retries. Monotonic; resets only when the process starts | a value that can fall; "events in the last minute" — the consumer computes rates |
| **gauge** | the current level of something: queue depth, bytes held, workers busy, an age, a 0/1 state | a count of events since start |
| **histogram** | a distribution the consumer must aggregate across nodes: latency, sizes, keys per transaction | a single current value |
| **state set** | a state from a closed set: one gauge, labelled `state`, 1 on the current value and 0 on the others | an open set of states |
| **info** | facts about the node, as labels on a gauge fixed at 1: `build_info{version, commit}` | anything that changes while the process runs, save the roles of [RFC 15](rfc-15-topology.md) |

- **Summaries, and quantiles computed in the process, are not used**: they cannot
  be aggregated across nodes or shares, and the question is always "p99 across
  the installation".
- A ratio is exported as a gauge only when its parts are not metrics already.
  Where both are counters, both are exported and the consumer divides.
- **A counter an alert reads is exported from start, at zero**, for every value of its
  closed labels: every bug counter of §7 and every counter its owner calls an alert.
  Other series appear on first use. A counter that first appears at 1 is invisible to
  a rate, so an "any increase" alert would miss the first event.

### 2.4 Labels

A label's values **MUST** be a set whose size is known before deployment. There
are three kinds of allowed label, and a list of forbidden ones.

1. **Closed sets**, named in the owning RFC's table — for example `op`, `protocol`,
   `outcome`, `result`, `reason`, `kind`, `direction`, `state`, `role`, `cause`,
   `stage` (the pipeline stages of [RFC 20 §4.1](rfc-20-adapters.md#4.1%20One%20pipeline)),
   `pool` (RFC 24's fixed pools). Values are
   lower-case words; an error label takes the names of the closed error set it
   reports ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values),
   [RFC 4 §4.8](rfc-4-remote-tier.md#4.8%20Errors%20are%20a%20closed%20set)). The
   `protocol` label takes `nfs3`, `nfs4` or `smb` — minor versions are not
   separate values — and NFS's auxiliary services (MOUNT, NLM, NSM) and
   the port mapper count under `nfs3`.

   **One name per concept across the set.** `outcome` is the class of an answer
   (`ok`, `error`, `delay`); `result` is `ok` or the name from a closed error set;
   `reason` is why something was refused or dropped; `cause` is the cause an error
   value carries.
2. **Entity labels**, whose values are configured objects: `store`, `namespace`,
   `journal`, `location`, `transform`, and `node`, only on a metric about another
   node (a replica, a peer's lease). Any metric may carry them.
   Each metric exports at most **64** values of each entity label per node, chosen in the
   order the entities were added and never re-chosen while the process runs; later ones
   are summed into `_other`, which only ever adds, and the node raises
   `metrics_label_capped` naming the metric (§3.9). When an entity is removed, the
   component that created its series deletes them, freeing its place. An entity label
   takes the object's name, not an internal ID. A share re-homed into a namespace of its
   own ([RFC 27](rfc-27-namespace-migration.md)) makes `namespace` grow with shares, so
   on namespace-labelled metrics ([RFC 9 §10](rfc-9-gc.md#10.%20Observability)) the cap is
   the bound that holds.
3. **`share` and `shard`, on gauges only**, under the worst-n rule of §2.5.

**Forbidden on every metric:** `client`, `principal`, `user`, `group`, `file`,
`path`, `name`, `session`, `connection`, `request`, `handle`, any key, material or
snapshot ID, and any value a client or a user chooses. They are unbounded, and
some are sensitive. Who did something is an event (§4); which file, a log line or
a span attribute (§5, §6).

**No share or shard label on any counter or histogram.** This generalises
[RFC 16 §8.1](rfc-16-metadata-store.md#8.1%20Metrics): at 10⁴ shares, a share
label on `vfs_op_seconds` multiplies its 8,500 series by 10⁴. A per-share count of
events is read through the management API from the component that keeps it, or
from traces.

> decision: share is a gauge-only label, even for counters an operator would
> like per share — refusals by share, bytes written by share. A counter per share
> is 10⁴ series for each label combination, and a counter cannot be cut to the
> worst few without losing the rate of the others. RFC 24 already names a share
> refused by its cap in a rate-limited log line for the same reason
> ([RFC 24 §10](rfc-24-resources.md#10.%20Observability)). Overturned by an
> installation profile with few shares, where the label would cost little — and
> then as a setting that turns it on, never as a default.

### 2.5 Cardinality

**Per-share gauges are exported for the worst shares only.** A gauge a component
keeps per share — dirty bytes, oldest unoffloaded age, history bytes, newest
backup age, re-home refs left — is exported:

- labelled `share`, for the `metrics.top_shares` shares worst by that gauge
  (default 20, a setting of [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings));
  "worst" is the owning RFC's direction — largest for a backlog or an age,
  smallest for a headroom — and the selection is recomputed at every export;
- and, without a `share` label, as the sum over every share and, for an age or a
  headroom, the worst value, so totals and alerts need no query across series and
  survive the cut.

Every share's values are readable through the management API, from the same
source. The same rule applies to `shard`. This is the "stated rule" that
[RFC 8 §14](rfc-8-engine.md#14.%20Observability) and
[RFC 16 §8.1](rfc-16-metadata-store.md#8.1%20Metrics) refer to.

**Series budget.** At the reference profile — 10⁴ shares, every entity label at
its cap of 64 values, every protocol in use — a node **MUST** export no more than **50,000** series in
all and no single metric more than **10,000**, histogram buckets included. Both
are fixed design limits, checked by counting after the reference workload (§12);
a table whose metric cannot fit is a defect in that table. `vfs_op_seconds` sets
the scale: about 45 operations × 3 protocols × 3 outcomes × 21 series per
histogram (18 boundaries, `+Inf`, `_sum`, `_count`) is 8,505.

> ponytail: the budget is checked by one count at one profile. Past it, add a
> per-metric series limit in the exporter that drops and counts new series,
> when a deployment shows a metric growing in a way the profile did not exercise.

![The operation-latency histogram costs about 8,500 series with op, protocol and outcome; a share label would multiply that by 10,000 to about 85 million; per-share figures are gauges exported for the worst 20 shares plus an unlabelled sum, and every share is read through the management API](img/rfc25-labels.svg)

### 2.6 Histogram buckets

A histogram uses one of four fixed bucket sets, so the same quantity aggregates
across metrics and nodes:

| Set | Boundaries | For |
| --- | --- | --- |
| `request` | 100 µs, 250 µs, 500 µs, 1 ms, 2.5 ms, 5 ms, 10 ms, 25 ms, 50 ms, 100 ms, 250 ms, 500 ms, 1 s, 2.5 s, 5 s, 10 s, 30 s, 60 s | anything a client request waits on: operations, KV calls, remote calls, recalls |
| `background` | 10 ms, 100 ms, 1 s, 5 s, 10 s, 30 s, 60 s, 5 min, 15 min, 1 h, 6 h | passes, handovers, takeovers, freezes, snapshot completion |
| `size` | 512 B, 2 KiB, 8 KiB, 32 KiB, 128 KiB, 512 KiB, 2 MiB, 8 MiB, 32 MiB, 128 MiB | payload, block and transaction sizes in bytes |
| `count` | 1, 2, 4 … 4,096 | keys per transaction, files per commit, refs per batch |

The `request` set ends past 30 s, the default request deadline
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)), and 35 s, the
SMB break deadline, so a request that runs out its deadline lands in a finite
bucket.

> ponytail: fixed buckets, 18 at most. Move to sparse exponential histograms when
> the exposition format and the consumers in use read them, which removes the
> choice and the bucket series together.

### 2.7 One event, one owner

Every event is counted in exactly one place, by the component that decides it,
and other RFCs link to that table rather than count it again. This extends what
[RFC 3 §2.12](rfc-3-syncer.md#2.12%20What%20the%20syncer%20makes%20observable),
[RFC 15 §10](rfc-15-topology.md#10.%20Observability) and
[RFC 17 §7](rfc-17-vfs.md#7.%20Observability) already say for their own
metrics. Two metrics counting the same event drift apart, and an alert on one
misses what the other saw. §11 lists the duplicates in the set today.

### 2.8 Export

- Metrics are **pulled**, in a standard text exposition format, from a listener of
  their own that serves nothing else; [RFC 23](rfc-23-control-plane.md) owns its
  address and authentication.
- An export **MUST** read counters and snapshots that components keep anyway, as
  [RFC 3 §2.12](rfc-3-syncer.md#2.12%20What%20the%20syncer%20makes%20observable)
  requires of the syncer: it takes no lock a request path needs, and its cost does
  not grow with work in flight. An export that needs a component's state it cannot
  read within 5 s omits that component's series and counts it in
  `health_export_partial_total`, rather than block.
- **Two ways to emit, one naming rule.** A leaf component that depends on no metrics
  library (the journal, the syncer) keeps counters in a `Stats` snapshot,
  and the component that composes it exports that snapshot under the leaf's prefix
  through one read-through collector. The carver keeps no counters at all: its
  caller derives the `carver_` series from what `Cut` hands back
  ([RFC 2 §2.3](rfc-2-carver.md#2.3%20What%20a%20pass%20makes%20observable)). Any other component is handed the registry in its
  `Deps` and records into series it resolved at construction (§2.9). Either way the
  name belongs to the RFC that owns the prefix, not to the code that exports it.
- An export carries no file name, no user content and no secret (§2.4, §5.4). So
  it is not audit data; whether it is authenticated is still RFC 23's choice,
  since share and store names are.

### 2.9 Recording

- A series a request touches is resolved once, at construction or when its entity is
  added; recording is an atomic add or an observe, with no label lookup and no
  allocation.
- The registry refuses a metric whose name, unit word, prefix or labels break §2.1–§2.4
  when it is registered, so every unit test that builds a component checks its names.

## 3. Health

### 3.1 Conditions

A **condition** is the one way a component says something is wrong. It carries:

| Field | Means |
| --- | --- |
| name | from the registry (§3.9): `store_put_unhealthy`, `journal_full`, `node_fenced` |
| subject | the scope and the object: `store s3-eu`, `share profiles`, `node S1` |
| severity | `degraded` or `unhealthy`, from the registry's rule, which may read the cause |
| cause | the closed error or reason the owner reports — `ErrDenied (access)`, `ErrTransient` |
| since | when the condition was raised, in store time |
| detail | a short constant message and the figures that matter (bytes held, refusals since) |

- A condition names its subject by name and ID, and a file by its FileID, never by
  path: a health report is not where file names go (§5.4).
- A component reports its conditions when the evaluator asks, once per
  evaluation; a condition its source no longer reports is cleared. Nothing else
  reports a component's conditions, and no API raises or clears one, so a
  condition cannot be latched (§3.4). The Go shape is §8.2's `Source`.
- **Information is not a condition.** A grace period in progress, a snapshot being
  taken, a node draining at an operator's request: they appear in the health
  report's information section (§3.7) and change no status.

### 3.2 Severity

- **degraded** — every operation the subject offers is still answered as its RFC
  specifies, but something it relies on is reduced: redundancy, headroom,
  timeliness, or a background guarantee such as offload, GC or backups. Left
  alone, it may become unhealthy.
- **unhealthy** — some class of operation the subject offers is refused, failing
  or at risk now: writes answered no space or held at capacity, reads of remote
  content failing, a node fenced, acknowledged content lost or unprotected.

The registry gives each condition its rule, and the rule may depend on the cause.
`store_put_unhealthy` is degraded with cause `ErrTransient`, since the journal
absorbs an outage shorter than its headroom
([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)), and
unhealthy with cause `ErrDenied` or `ErrDrift`, since writes waiting on it are
answered no space ([RFC 8 §11.1](rfc-8-engine.md#11.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included)).
A degraded condition that, by its nature, turns into an unhealthy one — an
offload stall that fills the journal — is two conditions, each raised by its own
evidence (`offload_stalled`, then `journal_full`), never one whose severity
creeps with a timer.

A refusal that is part of normal operation — an `ErrDelay` under pacing, a
quota refusal, a recall in progress — is not a condition. It is counted by its
owner.

### 3.3 Scopes, and how they combine

| Scope | Raised by | Applies to |
| --- | --- | --- |
| store | the syncer, the remote store | itself, and every share whose flow uses it |
| namespace | GC, transforms, the claim | itself, and every share in it |
| journal | the journal, the engine | itself, and every share it serves |
| share | the engine, snapshots, backups, the service | itself |
| node | the engine, configuration, resources, the metadata store, this RFC | itself |
| installation | the metadata store's loss, a peer's loss **(cluster)** | itself |

- **A subject's status is the worst severity among its conditions** — its own and
  those that apply to it — and healthy with none.
- **A share's status reads its own conditions and those of the store, namespace
  and journal behind it.** So a store denial shows on `profiles`, named as the
  store's, with no second condition raised.
- **A share's conditions never set its node's status.** The node reports how many
  of its shares are degraded and unhealthy, and lists the worst. A node serving
  10⁴ shares is not unhealthy because one of them is.
- **The installation's status is the worst of every node's, every share's and its
  own.** It is the one answer to "is anything wrong anywhere".

> decision: status is the worst severity, never a count — no "three degraded
> make unhealthy". A worst-of status can always be explained by naming one
> condition; a counted one cannot, and its threshold is a number no measurement
> settles. Overturned by an incident where several degraded conditions together
> failed clients while none alone did; the fix then is a condition for that
> combination, raised by the component that sees it.

### 3.4 A condition is derived, never latched

- A condition **MUST** be derived from current evidence at every evaluation and
  cleared when the evidence no longer holds. It **MUST NOT** be a flag that stays
  set until something clears it: [RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)
  shows how a latched "down" suppresses the attempts that would see the recovery.
- Every condition clears on its own once its cause is removed
  ([RFC 0 §10.2](rfc-0-data-lifecycle.md#10.2%20No%20state%20requires%20intervention%20to%20leave)),
  except the states RFC 0 names as waiting for an operator: a lost journal device,
  a lost metadata store, a node started from a copied image, and **(cluster)** a
  relinquished shard whose replicas are lost. Those clear when the operator acts.
- Damping is the owner's: a condition that flaps is held by its owner's rule, as
  the syncer's hold-down holds a store
  ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)).
  This RFC adds no hysteresis of its own; the alert's delay (§7) is what keeps a
  short condition from paging.
- Each raise logs once at `Warn` (`Error` for an unhealthy one) and each clear
  once at `Info`, with the condition's name, subject, cause and duration, and is
  counted in `health_condition_transitions_total{condition}` (§10).

> decision: silencing is the alert router's, not the node's. Overturned by a
> deployment with no alert router that still needs a silence.

> decision: `start_failed` clears only when the process exits, and is still not
> a latch under O7. Its evidence is a start that stopped at a named step, and
> that evidence holds for as long as the process lives, because a failed start
> does not retry ([RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop)):
> nothing could clear it sooner without lying. A restart is a new evaluation,
> which derives it again or not. Overturned if any start step ever retries in
> process; it must then clear when that step succeeds.

### 3.5 Liveness

**Liveness answers one question: is the process making progress?** It **MUST**
fail only when a fixed set of internal loops — each adapter's accept loop, the
request scheduler, each work-scheduler instance (one per engine journal, and
GC's) and the health evaluator — has
not completed a turn, or reported itself idle, within 60 s. An idle report is a
heartbeat, not a state: a waiting loop wakes at least every 30 s to report again, an
accept loop through an accept deadline, so a loop that wakes and stalls before its
turn misses its next report. It **MUST NOT** read a condition, a store, the network
or the disk.

> decision: an idle loop is live. The watchdog catches a loop stuck mid-turn, not one
> that never wakes: a lost wakeup with work queued passes liveness and shows as a queue
> that only grows. The work scheduler is the one loop that already reports more: it
> is idle only with nothing due ([RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler)), so a lost wakeup there fails
> liveness. Overturned by an incident where a lost wakeup needed a restart;
> then the loop reports its queue's oldest entry, and an old entry fails liveness.

> decision: no dependency fails liveness. A liveness failure asks the
> orchestrator to restart the process, and no dependency failure in this set is
> fixed by a restart: the journal recovers, every NFS client waits out a grace
> period, every SMB client reconnects, and the store is as unreachable as before.
> Overturned by a failure a restart demonstrably clears, which is then a bug to
> fix, not a probe to add.

### 3.6 Readiness

**Readiness answers whether the node should be sent new clients.** It fails only
while sending them elsewhere would help: the node can serve no share at all, or a
listener clients are sent to is down.

| Node state | Live | Ready | Status |
| --- | --- | --- | --- |
| starting: opening stores, recovering journals, re-applying existence | yes | no | starting |
| serving | yes | yes | from its conditions |
| in grace after a restart | yes | yes — clients must reach it to reclaim | from its conditions; grace is information |
| self-fenced: no write transaction committed for 30 s ([RFC 8 §11.2](rfc-8-engine.md#11.2%20Every%20condition%20in%20RFC%200%20%C2%A710%20has%20its%20engine%20behaviour%20here)) | yes | yes — reads continue | unhealthy, `node_fenced` |
| some shares refused: a journal device lost, a share's store denied | yes | yes | unhealthy for those shares |
| started from a copied image | yes | yes — reads of offloaded content continue | unhealthy, `copied_image` |
| metadata store lost: no share served until a recovery import | yes | no | unhealthy, `metadata_store_lost` |
| a listener lost (`adapter_listener_lost`) | yes | no — clients sent to it cannot connect | unhealthy |
| an adapter refused at configured (`adapter_refused`) | yes | yes — every node refuses the same setting, and the API that fixes it must stay reachable | unhealthy |
| a start step failed | yes | no | unhealthy, `start_failed` |
| draining: stop requested | yes | no | draining |
| watchdog stalled | no | no | — |

Every node runs both roles, so readiness has one rule: a node is ready once each
enabled adapter accepts clients or was refused at configured, the share list is loaded and, **(cluster)**, it has
joined the store. A shard it holds and cannot serve does not make it unready,
since sending clients elsewhere would not serve that shard either; it is the
shard's own condition, `shard_relinquished` or `replication_under_floor`
(§3.9). A rolling upgrade
([RFC 30 §5.1](rfc-30-kubernetes.md#5.1%20Rolling%20upgrade)) waits on those
conditions as well as on readiness, so the one readiness answer serves traffic
and the upgrade alike.

### 3.7 What the health answers report

[RFC 23 §2.5](rfc-23-control-plane.md#2.5%20Health%20and%20readiness%20endpoints)
serves three answers. Their content is this:

- **liveness** (`GET /health/live`): the one word `live` while §3.5's watchdog holds; a failed answer
  once it has not, and nothing else.
- **readiness** (`GET /health/ready`): one word, the node state from §3.6's table — `ready` while
  ready, otherwise `starting`, `draining`, or `not_ready` (the
  metadata store lost, a listener lost, or a failed start step) — and nothing else.
- Neither answer names a share, a store or a condition, so either can be served
  to a probe that holds no credential
  ([RFC 23 §2.5](rfc-23-control-plane.md#2.5%20Health%20and%20readiness%20endpoints)).
- **health** (`GET /api/v1/health`): the status of the installation, of this node and of each scope in
  it; every current condition with its fields (§3.1) and
  its alert class; the counts of shares by status and the worst 20 shares; and
  the information section — grace periods and their remaining time, snapshots and
  backups running, drains. With a share or store named, the same for that subject
  alone. It names shares and stores, so it needs the `viewer` role
  ([RFC 23 §5.2](rfc-23-control-plane.md#5.2%20Roles)).

### 3.8 Installation health (cluster)

**(cluster, [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile))**
On one node, the installation's status is computed in memory by §3.3's rule —
the worst of the node's, every share's and the installation scope's own
conditions — so a share left unhealthy by a denied store makes the installation
unhealthy on one node as in a cluster. With more than one:

- each node writes its node-scope conditions, and those of the shares it is
  primary of, into its node-status record
  ([RFC 23 §2.4](rfc-23-control-plane.md#2.4%20What%20a%20node%20reports)), only
  when they change and at most once a second, so health costs the store a
  bounded write rate;
- any node answers the installation's health by reading every node record; a
  node whose lease has lapsed is the installation condition `node_lost` naming it,
  and its last written conditions are shown as stale;
- a share's conditions come from its primary's record; a gateway that
  cannot reach a primary writes the condition RFC 15 already gives it
  ([RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)).

### 3.9 The condition registry

Every condition the set names, with the scope it is raised on, its severity rule
and its alert class (§7). Names not yet given by their owner are proposed here;
the owner adopts the name in its own text. A component adding a condition adds a
row.

| Condition | Owner | Scope | Severity | Alert |
| --- | --- | --- | --- | --- |
| `store_put_unhealthy` | [RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work) | store | degraded with cause `ErrTransient` or `ErrCorrupt`; unhealthy with `ErrDenied` or `ErrDrift` | ticket at 15 min; page at 5 min when unhealthy |
| `store_get_unhealthy` | [RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work) | store | unhealthy: reads of remote content fail | page at 5 min |
| `offload_stalled` | [RFC 8 §11.1](rfc-8-engine.md#11.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included) | share | degraded | ticket at 15 min |
| `file_offload_failing` | [RFC 8 §11.1](rfc-8-engine.md#11.1%20Health%20is%20derived%20from%20recent%20outcomes%2C%20offload%20included) | share, naming the FileID | degraded | ticket at 1 h |
| `existence_commit_failing` | [RFC 8 §5.2](rfc-8-engine.md#5.2%20Group%20commit%20is%20bounded%2C%20and%20retries%20only%20the%20files%20that%20conflict) | share, naming the FileID | unhealthy: the group commit has backed the file off, and its stability points answer `ErrDelay` until its existence commits | page at 15 min |
| `offload_age_exceeded` | [RFC 8 §11.4](rfc-8-engine.md#11.4%20How%20far%20behind%20offload%20is%2C%20is%20observable) | share | degraded | ticket at once |
| `work_queue_stalled` | [RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler) | journal for the engine, node for GC, naming the source and tenant | degraded: a run has outlived twice its run timeout, or entries have been due with none dispatched for 5 min | ticket at 15 min |
| `removal_stalled` | [RFC 8 §8.1](rfc-8-engine.md#8.1%20A%20removal%20is%20one%20transaction%2C%20then%20batches) | share, naming the FileID and the removal's kind | degraded; unhealthy for a clone, whose destination's writes are held | ticket at 1 h; page at 15 min for a clone |
| `journal_full` | [RFC 1 §6](rfc-1-journal.md#6.%20Capacity) | journal | unhealthy: writes held or refused at capacity | page at 5 min |
| `journal_sync_failed` | [RFC 1 §5.3](rfc-1-journal.md#5.3%20A%20failed%20sync) | journal | unhealthy: acknowledged writes lost | page at once |
| `journal_lost` | [RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model) | journal | unhealthy; waits for an operator | page at once |
| `stranded_export_failed` | [RFC 1 §3](rfc-1-journal.md#3.%20Interface), [RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model) | node | degraded | ticket |
| `node_fenced` | [RFC 8 §11.2](rfc-8-engine.md#11.2%20Every%20condition%20in%20RFC%200%20%C2%A710%20has%20its%20engine%20behaviour%20here) | node | unhealthy: writes answer retry-later | page at once — it already waited 30 s |
| `metadata_store_unhealthy`, `metadata_store_degraded` | [RFC 16 §8.3](rfc-16-metadata-store.md#8.3%20Health%20and%20debugging) | node | as named | page at 5 min; ticket |
| `metadata_store_lost` | [RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model) | installation | unhealthy; waits for an operator | page at once |
| `copied_image` | [RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model) | node | unhealthy; waits for an operator | page at once |
| `clock_unsynchronised` | [RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model) | node | degraded | ticket |
| `material_unavailable` | [RFC 5 §6](rfc-5-transforms.md#6.%20Observability) | namespace | unhealthy: behaves as an unavailable remote | page at 5 min |
| `config_generation_behind` | [RFC 13 §8](rfc-13-configuration.md#8.%20Observability) | node | degraded, past the 5 s propagation bound | ticket at 5 min |
| `provisioning_refused` | [RFC 13 §2.4](rfc-13-configuration.md#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) | node | degraded | ticket |
| `gc_hold`, `gc_deletion_backlog` | [RFC 9 §10](rfc-9-gc.md#10.%20Observability) | namespace | degraded | ticket |
| `gc_clock_skew` | [RFC 9 §10](rfc-9-gc.md#10.%20Observability) | node | degraded: raised while the local clock differs from store time by more than GC's clock skew limit, and the deleter stops; cleared once it is back within the limit | ticket |
| `gc_audit_behind` | [RFC 9 §10](rfc-9-gc.md#10.%20Observability) | namespace | degraded: raised when a hash range was not covered within its period, or the derived rate is above the scheduler's cap; cleared when every range is covered within its period and the rate is under the cap | ticket |
| `gc_lost_refs` | [RFC 9 §10](rfc-9-gc.md#10.%20Observability) | namespace, naming the file and offset | unhealthy: raised while any Lost ref is recorded and not yet dropped; cleared when the last is dropped, never by an operator's acknowledgement | page at once |
| `namespace_count_underflow` | [RFC 7 §11.2](rfc-7-namespace-metadata.md#11.2%20Observability) | share, naming the directory's FileID | degraded | ticket |
| `snapshot_policy_skipping` | [RFC 12 §6](rfc-12-snapshots.md#6.%20Observability) | share, at 3 skipped ticks | degraded | ticket |
| `backup_failing`, `backup_location_mismatch` | [RFC 26 §7](rfc-26-catalog-backups.md#7.%20Observability) | share; location | degraded | ticket |
| `directory_unreachable` | [RFC 18 §9.2](rfc-18-identity.md#9.2%20Metrics) | installation | degraded within the stale grace; unhealthy past it, since sign-ins are refused | ticket; page at 5 min when unhealthy |
| `adapter_listener_lost` | [RFC 20 §3.7](rfc-20-adapters.md#3.7%20Health%20and%20probes) | node, naming the adapter and address | unhealthy: a listener's socket failed while serving | page at 5 min |
| `adapter_refused` | [RFC 20 §3.7](rfc-20-adapters.md#3.7%20Health%20and%20probes) | node, naming the adapter and its error | unhealthy while an enabled adapter's settings fail at configured; cleared when it serves or is disabled | page at once |
| `start_failed` | [RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop) | node, naming the step and its error | unhealthy; held until the process exits, since a failed start does not retry, and a restart starts it again | page at once |
| `sidecar_failed` | [RFC 20 §3.5](rfc-20-adapters.md#3.5%20Sidecars) | node, naming the sidecar | degraded: the adapters keep serving | ticket |
| `memory_limit_below_floors`, `fd_limit_low`, `memory_near_limit` | [RFC 24 §10](rfc-24-resources.md#10.%20Observability) | node | degraded | ticket |
| `metrics_label_capped` | §2.4 | node | degraded | ticket |
| `event_spool_high` | §4.6 | node | degraded, above 80% of the spool bound | ticket |
| `event_gap` | §4.4 | node, naming the stream (any stream) | degraded | page for the access stream while any share's audit policy is on; ticket otherwise |
| `debug_logging_on` | §5.2 | node, naming the component | degraded | ticket |
| `replication_under_floor` **(cluster)** | [RFC 10 §14](rfc-10-journal-replication.md#14.%20Observability) | share | degraded | page at 5 min |
| `shard_relinquished` **(cluster)** | [RFC 10 §14](rfc-10-journal-replication.md#14.%20Observability) | share | unhealthy; waits for a replica or the operator | page at once |
| `node_lost` **(cluster)** | [RFC 11 §9](rfc-11-ownership.md#9.%20Failure), [§3.8](#3.8%20Installation%20health%20%28cluster%29) | installation | degraded while its shards fail over | page at 5 min |
| `lock_grant_unacknowledged` | [RFC 14 §14](rfc-14-open-state.md#14.%20Observability) | node, naming the client | degraded | ticket |
| `recall_deadline_misses` | [RFC 14 §14](rfc-14-open-state.md#14.%20Observability) | node | degraded while recalls revoked at their deadline exceed 1 per minute for 15 min (proposed) | ticket |
| `adapter_record_near_bound` | [RFC 14 §14](rfc-14-open-state.md#14.%20Observability) | node, naming the protocol | degraded while any adapter record written in the last hour exceeds 3 KiB of its 4 KiB bound | ticket |
| `adapter_record_unreadable` | [RFC 20 §3.7](rfc-20-adapters.md#3.7%20Health%20and%20probes) | node, naming the protocol | degraded while adapter records fail to decode, the usual cause a rolling upgrade across record versions | ticket at once |
| `grace_stuck` | [RFC 14 §14](rfc-14-open-state.md#14.%20Observability) | share, naming the shard | unhealthy: a shard has been in grace past twice the lease period | page at 5 min |

## 4. Event streams

### 4.1 Four streams

| Stream | Emitted by | Carries | Selected by |
| --- | --- | --- | --- |
| `access` | the filesystem service, where it consults the chokepoint ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)) | one authorisation decision | the share's audit policy and the file's audit entries ([RFC 19 §3.6](rfc-19-authorization.md#3.6%20Audit%20entries%20select%20events)) |
| `quota` | the filesystem service, where it enforces quota ([RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)) | a threshold crossed, a soft limit exceeded, a grace expired, a hard refusal, a return under the limit | always |
| `logon` | the authenticator, at each logon attempt ([RFC 18 §3.1](rfc-18-identity.md#3.1%20The%20authentication%20interface)) | one sign-in, over either protocol: success or failure, flavour, principal, client address; never the credential | always |
| `admin` | the control plane's audit records ([RFC 23 §6](rfc-23-control-plane.md#6.%20The%20audit)) | one management action or refusal: actor, target, change, outcome — a governance-lock override, a statement that a location is lost, a share's audit policy changed | always |

The administrative stream is the export of the management audit that other RFCs
record operator actions in ([RFC 13 §9](rfc-13-configuration.md#9.%20Test%20plan),
[RFC 12 §6](rfc-12-snapshots.md#6.%20Observability)). Unlike the others it
starts from durable records: RFC 23 commits each audit record in the
transaction of the change it records, keeps it for `audit.retention`, and owns
its fields. This section owns only how those records are read (§4.3).

Alarm entries select access events like audit entries do
([RFC 19 §2.2](rfc-19-authorization.md#2.2%20Entry%20kinds)): a record says which
selected it, and this RFC defines no second consumer that would need them apart.

### 4.2 What a record carries

A record of the `access`, `quota` and `logon` streams is one JSON object (RFC 8259)
on one line. The event's own fields are
[RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)'s; the sink stamps on the rest. An
`admin` record is RFC 23's audit record, with its fields, named and ordered by its
commit position ([RFC 23 §6](rfc-23-control-plane.md#6.%20The%20audit)); none of the
table below applies to it (§4.3).

| Field | Set by | Means |
| --- | --- | --- |
| `v` | sink | the record format's version, 1; a consumer refuses a version it does not know |
| `stream` | sink | `access`, `quota`, `logon` |
| `node`, `boot`, `seq` | sink | the node's ID, a random ID drawn at each process start, and a sequence number counted from 0 per stream and boot; `(node, stream, boot, seq)` names a record uniquely and shows any gap |
| `time` | event | the event's time, RFC 3339 in UTC with nanoseconds |
| `trace_id` | sink, from the call's context | the trace the decision ran in (§6), when sampled |
| `share` | event | the share's ID and name |
| `client`, `protocol` | event | the client's ID and address, and the protocol |
| `identity` | event | the principal ID, the flavour and whether authenticated, as [RFC 18](rfc-18-identity.md) resolved them — not a uid alone, which two principals may share |
| `op`, `file`, `name` | event | the operation, the FileID, and the name acted on for a namespace operation |
| `want`, `allowed`, `selected_by` | event | the rights asked, the decision, and `policy` or `entry` |
| logon fields | event | the outcome (success, or the authenticator's error, `throttled` when the attempt was refused unchecked), the flavour, the principal name as presented and, on success, the principal ID; with `client` and `protocol` above. Never a response, ticket, hash or key |
| quota fields | event | scope (`user`, `group`, `project`, `share`), subject, resource (`bytes`, `files`), kind, used, soft, hard, grace end, and for a hard refusal the writer's identity and the count coalesced (§4.4) |

- A name is written as UTF-8 when it is valid UTF-8, and otherwise as base64 under
  `name_b64`, so no byte a client chose can break a line or a parser.
- **A record carries no file content and no secret.** It carries a file name only
  in the `access` stream and only for namespace operations, where the name is the
  thing decided.

[RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) shows the `AccessEvent` type and
lists what a quota event is for, but not its fields; §11 asks it to add
`QuotaEvent` with the fields above.

### 4.3 From the emit point to the spool

![The service calls the sink at quota enforcement and with each authorisation decision, the authenticator at each logon, and neither waits; the sink stamps node, boot and sequence and puts the event in a fixed ring per stream, dropping and counting when full; one writer per stream appends batches to spool files every second, writing a gap record for anything dropped; the spool is bounded by age and bytes and is read by a shipper tailing the files or by an auditor through the management API; the admin stream is the control plane's audit, pulled by position with no ring and no spool](img/rfc25-events.svg)

1. **Emitters call the sink and return.** The sink is a concrete type with one method
   per stream — `Access`, `Quota`, `Logon` — each taking a pointer to the event; a nil
   sink is the null sink, and with no policy no event is built. A call copies the event,
   and the name's bytes, into a ring slot and **MUST NOT** allocate, take a lock a
   request path also takes, or wait ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)).
   Because the methods are concrete, the event stays on the caller's stack;
   `testing.AllocsPerRun` of each is 0 (§12).
2. **One ring per stream, of fixed size**: 16,384 entries for `access` and 4,096
   each for `quota` and `logon`, each entry at most 1 KiB. A slot holds the principal ID
   and flavour, not the group list (§4.2). A name longer than the slot's room is written
   truncated, with `name_truncated: true`, never dropped. The rings are allocated at start, so
   they are fixed memory outside RFC 24's pools: at most 24 MiB per node,
   whatever the load.
3. **One writer per stream** takes entries in order and appends them to the
   stream's current spool file, in batches, syncing the file and making it visible
   every second.
4. **Spool files** are rotated at 64 MiB or one hour, whichever comes first, and
   named by stream and a file number, zero-padded to 20 digits, one more than
   the last file's. The next number is kept in a small file in the spool
   directory, synced before a spool file is created, so names sort in write
   order across restarts and after retention has deleted every file, and the
   oldest file is the lowest number. A boot ID is random and a sequence number
   restarts with it, so neither can order files.
5. **The `admin` stream has no ring and no spool.** It is RFC 23's audit, read in commit
   order through the management API by position; the consumer keeps its position.
   Nothing is lost while the consumer is away, short of `audit.retention`.

> ponytail: the admin stream is pulled, not spooled. Spool it on the node, with a cursor
> record per node, when a deployment's shipper can only tail files and cannot page an API.

### 4.4 Bounds, drops and gap records

- **When a ring is full, the new event is dropped**, counted in
  `events_dropped_total{stream, reason="ring_full"}`, and remembered as a range of
  lost sequence numbers. The writer writes a **gap record** in sequence
  position: after every accepted record numbered below the range and before every
  record above it, and at the end of its next batch when no later record has
  arrived, so a gap is in the spool within a second of the loss whether or not
  the stream goes on. It carries `stream`, `kind: "gap"`, the first and last
  sequence numbers lost, the count, the time range, and the reason. So a consumer finds the loss in
  the stream itself, not only in a metric it may not watch.
- The same holds for every other loss: `spool_full` when the writer cannot
  append, `trimmed` when the byte bound deletes a file (§4.6), and at start,
  `restart`, a gap record with no count for the events of the last unsynced
  second before the previous process ended. The
  `admin` stream is not spooled and loses nothing here.
- A gap in any stream raises `event_gap` naming the stream (§3.9), cleared an
  hour after its last gap.
- **Quota events are coalesced.** A transition — threshold crossed, soft limit
  exceeded, grace expired, back under — is emitted once per transition. Hard
  refusals are coalesced per (share, scope, subject, resource): the first after a
  minute with none is emitted at once, and every later one is counted into a
  summary record at the end of each 60 s window, counted from that first, that
  saw any. A writer hammering a full quota for ten minutes produces eleven
  records — the first, then one a minute — not one per write.

> decision: the access stream drops rather than blocks. An access event is not
> worth an operation: a sink that could block would let a slow consumer stall
> every client of every share, which is what
> [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) forbids. The loss is bounded
> and visible: a gap record in the stream, a counter, and a page while an audit
> policy is on. Overturned by a compliance profile that requires an operation to
> fail when it cannot be audited — then as a share setting that refuses the
> operation with `ErrDelay` while the ring is full, never as the default.

### 4.5 Durability and order

- **An event is not durable with its operation.** It reaches the spool within a
  second of the decision; a crash in between loses it, and the next start writes
  the `restart` gap record. Writing the event in the operation's own transaction
  would put a store write on every audited read.
- **Order is per node and stream** in the spooled streams; the `admin` stream is
  ordered by the audit's commit position (§4.3). Within one `(node, stream, boot)` sequence
  numbers rise with the order the sink accepted events, which is the order of
  the decisions on that node. Across nodes there is no order but `time`
  **(cluster)**: a consumer merges by time and names records by
  `(node, stream, boot, seq)`.
- **(cluster)** A quota transition is seen by every primary that writes for the
  subject, so it may be emitted by more than one node within the clock-offset
  bound. A consumer that wants one record per transition suppresses repeats of
  (share, scope, subject, resource, kind) within a minute. That is time-window
  suppression, not an identity: a subject that crosses a limit, falls back under
  and crosses it again within the minute loses the second crossing to it. A
  subject's current quota state is read through the management API, not
  rebuilt from the stream.

> ponytail: quota transitions have no identity shared across primaries. Give
> each limit a transition counter in the metadata store, raised in the commit
> that moves usage across it and carried on the event — the soft limit's
> first-exceeded time is such a record already — when a consumer needs exact
> transitions rather than suppressed ones.

> ponytail: no hash chain across records. A sequence number shows a deleted
> record, not an edited one. Add a chained digest per spool file when a
> compliance profile requires tamper evidence on the node itself; until then the
> answer is a shipper that moves records to write-once storage promptly.

### 4.6 Retention

- The spool lives under `events.path`, a host bootstrap path
  ([RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap)),
  which **MUST NOT** be on a journal's filesystem: a start that finds it there is
  refused, as RFC 13 refuses its other scratch paths. The spool would otherwise
  take the room the journal's capacity bound counts on.
- A spool file is **retired** once it is older than `events.retention` (default
  7 days, proposed), and **trimmed**, oldest first, while the spool exceeds
  `events.max_bytes` (default 4 GiB per node, proposed). Both are settings of
  [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings).
- Trimming writes a `trimmed` gap record and counts the records in
  `events_dropped_total{reason="trimmed"}`: a file deleted for room before its
  age is a shipper falling behind. Retiring writes no gap record and raises
  nothing: it is the spool working as designed, and on a node with steady
  traffic it happens every hour.
- `event_spool_high` is raised above 80% of `events.max_bytes` — the warning that
  comes before a trim.

> decision: retirement by age is not a loss. The node cannot tell a file a
> shipper has read from one it has not, so it takes age as read: a shipper a
> whole `events.retention` behind — a week by default — is an outage its own
> monitoring sees long before. Overturned by a shipper that acknowledges what it
> has read, which would let the node write a gap record for an unread file it
> retires.

> decision: the spool is a buffer for a shipper, not the archive. Keeping audit
> records for months is the downstream store's job, with its own access control
> and immutability; a node that kept them would need both, and a quota on a disk
> it does not own. Overturned by a deployment with no log pipeline that must still
> keep audit records — then by a backup-location export of the spool, not by a
> larger spool.

### 4.7 Export

The spool files are the export: JSON lines, one record per line, rotated and named
in order (§4.3). A shipper tails them; the format needs no DittoFS code to read.
The management API also serves the spooled streams' records, filtered by share,
time and sequence range, read from the same files
([RFC 23 §6](rfc-23-control-plane.md#6.%20The%20audit) owns the endpoint,
`GET /api/v1/events/{stream}`); the `admin` stream is the audit itself (§4.3).

Events **MUST NOT** be exported as metrics or written to the log: the counters of
§10 count them, but no metric label or log line carries a record's identity, file
or name.

> ponytail: one export, files. A push exporter to a syslog collector or a trace
> pipeline's log channel is added when a deployment cannot run a shipper on the
> node — a managed appliance — and then as a second reader of the same spool,
> never instead of it.

### 4.8 Access control

Audit records say who opened what; they are among the most sensitive data the
server holds.

- Spool files and their directory are readable and writable only by the service's
  own account (modes 0600 and 0700), and the node refuses to start if
  `events.path` is readable by others.
- **Reading the `access`, `logon` and `admin` streams through the management API requires
  the right to read the audit**, which [RFC 23 §5.2](rfc-23-control-plane.md#5.2%20Roles)
  gives to `admin` only. Reading `quota` requires `operator`, the role that
  manages quotas. A `viewer`, who may read health detail, reads neither.
- Changing a share's audit policy is a management action, so it is itself an
  `admin` record.
- The streams are never served unauthenticated, never in a health answer, and
  never in a support bundle unless the operator who builds it holds the audit
  permission and asks for them.

## 5. Logs

### 5.1 Levels

| Level | For | Example |
| --- | --- | --- |
| `Error` | something failed that a person must look at: lost or corrupt content, an invariant breach, a bug, an unhealthy condition raised | a Lost read; an Nlink underflow; a handler panic |
| `Warn` | a degraded condition raised, work refused at a rate-limited summary, a retry budget exhausted | a store's hold-down; a share refused by its cap |
| `Info` | a lifecycle change, one line per change: start, stop, a share added, a primary changed, configuration applied, a condition cleared | `share added` |
| `Debug` | per-request detail, and every expected client error | `ErrNoEntity` on a lookup; a stale handle |

Expected errors — no such file, access denied, exists, not empty, stale handle —
are `Debug`, as [RFC 16 §8.2](rfc-16-metadata-store.md#8.2%20Logs%20and%20events)
and the project's rules have it: a client can produce them at any rate.

### 5.2 Volume is bounded per incident, not per request

- **No line at `Info` or above is written per client request.** What happens per
  request is a counter.
- A fault that lasts is logged as [RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)
  logs a store: a line when it starts, a summary at a fixed interval while it
  lasts with the counts since the last, and a line when it ends with the totals.
- A line that names something noisy — a share at its cap, an address at its
  connection quarter, a client sending unmappable IDs — is rate-limited to once a
  minute per subject, with the count suppressed.
- On a request path a `Debug` line is guarded by `Enabled`, because a logging
  call's arguments are boxed before the level is checked:

  ```go
  if s.log.Enabled(ctx, slog.LevelDebug) {
  	s.log.LogAttrs(ctx, slog.LevelDebug, "lookup refused", slog.Uint64("file", uint64(f)))
  }
  ```
- `Debug` turned on through the management API, for one component at a time, turns
  itself off after 15 minutes unless renewed. A level set in configuration or the
  environment stays, and the node raises `debug_logging_on` while it does.

> decision: debug turned on at run time expires; debug set at start is visible as a
> condition. Debug lines carry file names (§5.4) and cost a line per request; one
> left on after an incident fills the disk and the log store with user content,
> while a level set at start is a developer's or a test's choice and expiring it
> would silently drop what they asked for. Overturned by a debug level left in a
> deployment's configuration that filled a disk despite the condition.

### 5.3 Structure and correlation

Every line is one record with fixed keys, as JSON or as `key=value` text, chosen
at the node:

| Key | Always | Means |
| --- | --- | --- |
| `time`, `level` | yes | RFC 3339 UTC with milliseconds; the level |
| `msg` | yes | a constant string — no value interpolated, so lines group by message |
| `component` | yes | the owning prefix of §2.1 |
| `node` | yes | the node's ID |
| `trace_id`, `span_id` | on request-scoped and pass-scoped lines | §6 |
| `request_id` | on request-scoped lines | the route envelope's request ID ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)) or the adapter's own |
| `share`, `op`, `protocol`, `client` | where they apply | as in §4.2 |
| `error`, `cause` | on failures | the closed error and its cause |

- A **trace ID exists on every request**, sampled or not (§6.3), so a log line can
  always be joined to its request even when no span was exported. With tracing off
  the adapter still draws the trace ID and carries it in the request's own state,
  not as a context value.
- An error wrapped across components keeps its cause, and the line that reports it
  names the component that decided it, not the one that logged it.

### 5.4 What is never logged

No log line, span attribute, health detail, metric or event record carries:

- **a secret** — credentials, keys, tokens, passwords or their hashes, Kerberos
  tickets, session keys, NTLM or GSS blobs, signing keys
  ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets) for records; this rule for
  everything else);
- **file content** — payload bytes, extended attribute values, stream contents,
  symlink targets;
- **at `Info` or above, any name a user chose** — file and directory names, paths,
  symlink targets, stream names. A file is named by share and FileID. Names appear
  at `Debug` only, and in the `access` stream (§4.2);
- **a person's identifiers** — e-mail addresses, OpenID Connect issuer
  subjects and user names
  ([RFC 18 §8.2](rfc-18-identity.md#8.2%20OpenID%20Connect%20sign-in)). A
  principal is named by its principal ID.

Share, store and namespace names, principal IDs, client addresses and client IDs
are operational identifiers and may be logged at any level.

The check is a test, not a review (§12): a full conformance run with known
secrets and known names planted, and every line, span and record searched for
them.

## 6. Tracing

### 6.1 Spans

- The adapter opens one **request span** per wire request, in its pipeline, in the
  fixed order of [RFC 20 §4.1](rfc-20-adapters.md#4.1%20One%20pipeline); a
  compound has one span with a child per operation. A handler opens no span of its
  own.
- **Each service call opens a span** that parents the metadata store's and the
  engine's spans ([RFC 17 §7](rfc-17-vfs.md#7.%20Observability)); the metadata
  store opens one child per KV call
  ([RFC 16 §7.3](rfc-16-metadata-store.md#7.3%20Profiling)); the engine one per
  stage it waits on — journal sync, existence commit, fetch.
- Work done by another component is a child span; a wait inside one component is a
  span event on that component's span.
- Background work — an offload pass, a GC pass, a handover, a snapshot, a backup —
  opens a root span of its own. A pass that serves a request it was waiting on, a
  fetch joined by a read, *links* to that request's span rather than becoming its
  child, so a request's trace shows the wait without absorbing the pass.
- Span names are constant: `<prefix>.<Name>`, the prefix from §2.1's table —
  `vfs.Write`, `metadata.kv.Get`, `adapter.nfs4.COMPOUND`. Attributes are the
  operation's closed-set fields, sizes, the share and file by ID, the outcome and
  the error. §5.4 applies to every attribute.

### 6.2 Propagation across nodes

- **(cluster)** The route envelope ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope))
  carries a **trace context** — the trace ID, the parent span ID and the sampled
  flag, in the standard trace-context encoding — beside the request ID. The
  primary's service span is the child of the gateway's. The trace context
  **MUST NOT** enter the request ID, the dedup key or any durable record: a retry
  carries a new span and the same request ID.
- A client's own trace context is not read from the wire; NFS and SMB carry none.
  A trace starts at the adapter.
- Replication and node-channel calls **(cluster)** carry the context of the work
  that caused them.
- **(cluster)** The split between translating and deciding is one trace: the
  adapter's request span on the gateway node (`protocol`, operation, the neutral
  result), a child span for the forwarded call carrying the envelope's trace
  context, and the primary's service span, which carries the open-state step's
  neutral op, shard, result and whether it waited
  ([RFC 14 §14](rfc-14-open-state.md#14.%20Observability)). A recall and a
  lock waiter's grant open root spans linked to the request that caused them.
  No client, owner, file or principal is an attribute beyond what §5.4 allows.

### 6.3 Sampling and cost

- Tracing is off unless `tracing.endpoint` names a collector (a node setting of
  [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)).
  With it set, requests are sampled at the adapter at `tracing.sample_ratio`
  (default 0.01, proposed), and the decision travels with the context, so a trace
  is whole or absent.
- An unsampled span **MUST** cost no allocation and no clock read beyond the trace
  ID itself, so tracing off costs the request path nothing measurable. The
  conformance run counts allocations per operation with tracing off and on at
  ratio 0 (§12).
- Spans are exported in batches by a writer of their own, from a bounded queue
  that drops and counts (`health_spans_dropped_total`, §10) rather than block.

> ponytail: head sampling only. A slow or failed request is kept only if it was
> sampled. Tail sampling in the collector — keep every trace that ends in an
> error or past the p99 — needs every trace to reach the collector, so it means
> `tracing.sample_ratio` at 1: every request's spans exported, one per service
> call, KV call and engine stage, with the export queue's bound and drops
> limiting the cost instead of the ratio. Add it when an incident needs a failed
> request head sampling missed, after measuring that export at ratio 1 on the
> reference workload.

## 7. Alerting

**Alert on conditions, not on raw metrics, wherever a condition exists.** The
condition carries its subject and cause; an alert on a metric threshold has to
rediscover both.

- **page** — a person now. For: clients failing — an unhealthy condition that has
  lasted its delay; acknowledged data at risk — a journal sync failure, a Lost or
  corrupt extent with `data_loss`; a state only an operator can leave — the four of
  [RFC 0 §10.2](rfc-0-data-lifecycle.md#10.2%20No%20state%20requires%20intervention%20to%20leave);
  and a gap in an access stream an audit policy relies on.
- **ticket** — a person within a working day. For: a degraded condition that has
  lasted its delay, and every counter its owner calls a bug when it moves:
  `adapter_unmapped_errors_total`, `adapter_handler_panics_total`,
  `namespace_nlink_underflow_total`, `blockmeta_underflow_total`,
  `gc_verify_refusals_total`, `engine_ledger_mismatches_total`,
  `codec_verify_failures_total` net of the syncer's recovered re-gets
  ([RFC 4 §4.12](rfc-4-remote-tier.md#4.12%20What%20a%20store%20makes%20observable)).
- **nothing** — information: grace, drains, snapshots running.

Each condition's class and delay are in the registry (§3.9). An alert names the
condition, the subject, the cause and `since`, and links the troubleshooting
guide's entry for the condition. One condition is one alert, however many shares
inherit it: `store_put_unhealthy` on a store under 300 shares is one page naming
the store and the count of shares, not 300.

Alerts that read metrics directly are few and named here: a node that has stopped
exporting (the scraper's own), `health_status{scope="installation"}` away from
`healthy` for longer than the longest page delay, as a backstop for an alert
route that failed, and the bug counters above.

## 8. Components and lifecycle

`node.New` ([RFC 23 §2.1](rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components))
builds each component below once per process and hands it to its consumers as a
`Deps` field: no global, no registry lookup, no value carried in a context. A
field is a concrete type where there is one implementation, as in
[RFC 17 §2.3](rfc-17-vfs.md#2.3%20Dependencies%20and%20wiring). Start and stop
steps are [RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop)'s.

### 8.1 What the root builds

| Component | Package | Built by, owned by | Depends on | Injected as | Lifecycle |
| --- | --- | --- | --- | --- | --- |
| log handler | `internal/logger` | the root, first | settings | `Logger *slog.Logger`; each consumer adds `With("component", prefix)` (§5.3) | `New` only; a `Debug` level set through the API expires on a timer the handler owns (§5.2) |
| metrics registry | `pkg/metrics` | the root | build version and commit | `Metrics *metrics.Registry` | `New` registers only the process and runtime collectors; the metrics listener (§2.8) starts at start step 2 and stops after stop step 9, so a stuck stop can still be scraped |
| watchdog | `pkg/health` | the root | clock | `Watchdog *health.Watchdog`; each watched loop takes its own `*health.Loop` from `Watchdog.Loop(name)` and closes it when the loop ends — a listener on unbind, since [RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states)'s reconfiguration adds and removes listeners at run time | starts before start step 2, stops after stop step 9; the liveness answer reads it (§3.5) |
| health evaluator | `pkg/health` | the root; the node reads its `Snapshot` and writes node status (§3.8) | clock, logger, registry, watchdog, §3.9's table as Go, the `Source`s | injected into nothing: each component is a `Source`, added with `Add` and removed with `Remove` by the node, which owns the set of running components ([RFC 23 §2.1](rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components)) | `Start` before start step 2 takes `Watchdog.Loop("health")` and turns it after each evaluation, every second; `Close` after stop step 9 closes it |
| event sink | `pkg/events` | the root | clock, logger, registry, `events.path` | `Events *events.Sink`, to the filesystem service and the authenticator only | `New` opens nothing; `Start` after start step 2 and before step 6 opens the spool, writes the `restart` gap record (§4.4) and starts the writers; `Close(ctx)` after stop step 6 drains the rings until `ctx` ends, drops and counts the rest, and syncs the spool |
| tracer | none: the tracing API | the root builds the provider when `tracing.endpoint` is set (§6.3), a no-op provider otherwise | settings | `Tracer`, the tracing API's tracer type, in the `Deps` of every component that opens spans (§6.1): the adapters, the service, the metadata store, the engine and each background pass | the provider shuts down after stop step 6, flushing within the drain deadline; spans it cannot flush are counted (§10) |

### 8.2 The packages

Three packages, each with the narrowest surface its consumers need. There is no
tracing package: the `Deps` carry the tracing API's tracer type and the root
builds the provider.

```go
package metrics // pkg/metrics

// The vector and collector types are aliases of the client library's own,
// written clientlib here: no wrapper, so the surface is the library's.
type (
	CounterVec   = clientlib.CounterVec
	GaugeVec     = clientlib.GaugeVec
	HistogramVec = clientlib.HistogramVec
	Collector    = clientlib.Collector
)

type Buckets uint8

const (
	None       Buckets = iota // not a histogram
	Request                   // §2.6's sets
	Background
	Size
	Count
)

type Spec struct {
	Name    string   // without dittofs_; its prefix is in §2.1's table
	Help    string
	Labels  []string // refused if forbidden (§2.4); share and shard refused on counters and histograms
	Buckets Buckets  // histograms only
}

type Registry struct{ /* the client library's registry */ }

func New(version, commit string) *Registry

// Counter, Gauge and Histogram panic on a Spec that breaks §2.1–§2.4, so a bad
// name fails the first test that builds its component (§2.9).
func (r *Registry) Counter(s Spec) *CounterVec
func (r *Registry) Gauge(s Spec) *GaugeVec
func (r *Registry) Histogram(s Spec) *HistogramVec
func (r *Registry) Collect(c Collector) // read-through snapshots (§2.8), worst-n gauges (§2.5)
func (r *Registry) Handler() http.Handler
```

```go
package health // pkg/health

type Name string    // a condition's name; the constants are §3.9's table as Go
type Severity uint8 // Degraded, Unhealthy (§3.2)
type Scope uint8    // Store, Namespace, Journal, Share, Node, Installation (§3.3)

type Subject struct {
	Scope    Scope
	ID, Name string
}

// Condition carries no severity and no since: the registry's rule supplies the
// first from Name and Cause, and the evaluator the second.
type Condition struct {
	Name    Name
	Subject Subject
	Cause   error  // the closed error or reason (§3.1)
	Detail  string // a constant message and the figures that matter
}

// Source is what a component implements. The evaluator calls it once per
// evaluation; a condition the source no longer returns is cleared (§3.4).
type Source interface {
	Conditions(dst []Condition) []Condition
}

// Clock, here and in events, is the package's own subset of RFC 8 §2.1's Clock.

type Evaluator struct{ /* … */ }

func NewEvaluator(clk Clock, log *slog.Logger, m *metrics.Registry, wd *Watchdog) *Evaluator
func (e *Evaluator) Add(s Source)
func (e *Evaluator) Remove(s Source)
func (e *Evaluator) Start(ctx context.Context) error
func (e *Evaluator) Snapshot() Snapshot // what §3.7's health answer reports
func (e *Evaluator) Close() error

type Watchdog struct{ /* … */ }

func NewWatchdog(clk Clock) *Watchdog
func (w *Watchdog) Loop(name string) *Loop
func (w *Watchdog) Live() bool // every open loop turned or idle within 60 s (§3.5)

type Loop struct{ /* … */ }

func (l *Loop) Turn()  // a turn completed
func (l *Loop) Idle()  // waiting, with nothing to do; repeated at least every 30 s (§3.5)
func (l *Loop) Close() // the loop is gone and no longer counts
```

```go
package events // pkg/events

type AccessEvent struct{ /* RFC 17 §3.3's fields */ }
type QuotaEvent struct{ /* §4.2's quota fields */ }
type LogonEvent struct{ /* §4.2's logon fields */ }

// Sink is nil-safe: a nil *Sink is the null sink.
type Sink struct{ /* rings, writers, spool */ }

func New(clk Clock, log *slog.Logger, m *metrics.Registry, path string) *Sink
func (s *Sink) Start(ctx context.Context) error
func (s *Sink) Access(ctx context.Context, e *AccessEvent) // §4.3: copies, never waits
func (s *Sink) Quota(ctx context.Context, e *QuotaEvent)
func (s *Sink) Logon(ctx context.Context, e *LogonEvent)
func (s *Sink) Close(ctx context.Context) error
```

- **The event types live in `events`, not beside their emitters.** The sink's
  methods are typed, so a sink that took `vfs` and `identity` types would import
  both packages, while `vfs.Deps` imports the sink: an import cycle. So the sink
  and its three structs live in the leaf package, and
  [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) still fixes the access
  event's fields.
- **No library type name appears in this RFC's text.** The client library's
  types reach a component only through the aliases above.

### 8.3 On removal

A share's per-share gauges are computed at export from the live share list, so a
removed share leaves at the next scrape with nothing to delete. Its conditions
leave when the node removes its context's `Source`. No counter carries the
share, so none changes. A removed store, namespace or journal has its series
deleted by the component that created them (§2.4).

## 9. Invariants

| # | Invariant | Test (§12) |
| --- | --- | --- |
| O1 | Every metric's prefix has one owning RFC, its unit is in its name, and counters end `_total`. | §2.1, §2.2 names |
| O2 | No counter or histogram carries a share, shard, client, principal, file, path or connection label; no metric carries a value a client chose. | §2.4 forbidden labels |
| O3 | Every entity label is capped at 64 values per metric per node; per-share and per-shard gauges are exported for the worst `metrics.top_shares` and summed. | §2.5 budget |
| O4 | A node exports at most 50,000 series at the reference profile, no metric more than 10,000. | §2.5 budget |
| O5 | Every event is counted by one metric, in one RFC. | none: §11's audit, by review |
| O6 | A status is the worst severity among current conditions in scope; a share's conditions never set its node's status. | §3.3 share does not set node |
| O7 | Every condition is derived from current evidence and clears on its own, except the four states RFC 0 §10.2 names. | §3.4 never latched |
| O8 | Liveness reads no condition, store, network or disk. | §3.5 liveness |
| O9 | Readiness is false only while starting, draining, with the metadata store lost, with a listener down, or after a failed start step. | §3.6 readiness |
| O10 | In the `access`, `quota` and `logon` streams, an event hook never blocks or fails an operation; every event dropped leaves a gap record in its stream and a count. | §4.3 never blocks |
| O11 | In the `access`, `quota` and `logon` streams, every record names its node, stream, boot and sequence; within one stream and boot sequence numbers have no gap that is not a gap record, and a gap record sits in sequence position. | §4.3 never blocks; §4.5 restart |
| O12 | The spool is never on a journal's filesystem and is readable only by the service's account. | §4.6 retention; §4.8 access |
| O13 | The `access`, `logon` and `admin` streams are readable only with the right to read the audit, and never appear in a log, a metric or a health answer. | §4.8 access; §5.4 never logged |
| O14 | No log line, span, metric, health detail or record carries a secret or file content; no line at `Info` or above carries a user-chosen name. | §5.4 never logged |
| O15 | No line at `Info` or above is written per client request. | §5.2 per-request lines |
| O16 | The trace context never enters a request ID, dedup key or durable record. | §6.2 propagation |

## 10. Observability

What this RFC itself exports. Metric names are shown without the prefix.

| Answers | Metric | Type |
| --- | --- | --- |
| this node's status and the installation's, as a state set (`scope` = `node`, `installation`) | `health_status{scope, state}` | gauge |
| current conditions, by name and severity | `health_conditions{condition, severity}` | gauge |
| shares by status, on this node | `health_shares{state}` | gauge |
| condition raises and clears | `health_condition_transitions_total{condition, transition}` | counter |
| exports that omitted a component (§2.8) | `health_export_partial_total{component}` | counter |
| entity labels capped (§2.4) | `health_label_capped_total{metric}` | counter |
| spans dropped by the export queue (§6.3) | `health_spans_dropped_total` | counter |
| records accepted, by stream | `events_records_total{stream}` | counter |
| records dropped, by stream and reason (`ring_full`, `spool_full`, `trimmed`, `restart`) | `events_dropped_total{stream, reason}` | counter |
| ring occupancy against its size | `events_ring_entries{stream}`, `events_ring_entries_limit{stream}` | gauge, gauge |
| spool bytes against its bound, and the oldest file's age | `events_spool_bytes`, `events_spool_bytes_limit`, `events_spool_oldest_seconds` | gauge, gauge, gauge |
| time from accept to synced in the spool | `events_write_seconds{stream}` | histogram (`request`) |
| build | `build_info{version, commit}` | info |

`condition` takes the registry's names, a closed set; `health_conditions` is
exported at zero for every registered condition (§2.3). `events_dropped_total`
is the one count of lost events: it replaces the drop counters of RFC 7 and
RFC 17 (§11).

## 11. Audit of the set's tables

Every Observability section was read against §2. Each row is an edit for the
owning RFC; none is applied here. Rows already applied in their owning RFC have
been removed.

| RFC | Finding | Edit |
| --- | --- | --- |
| [RFC 17 §7](rfc-17-vfs.md#7.%20Observability), §3.3 | `outcome` on `vfs_op_seconds` has no stated values, and the full error set would cost about 70,000 series; `vfs_events_dropped_total` is the sink's; §3.3 defines no `QuotaEvent`, and says "planned observability RFC" | "`outcome` = `ok`, `error` or `delay`; the error itself is counted in `vfs_errors_total{op, error}`"; remove the drop row, "events dropped are RFC 25's `events_dropped_total`"; add a `QuotaEvent` type with RFC 25 §4.2's quota fields; "belong to RFC 25" |
| [RFC 24 §5.1](rfc-24-resources.md#5.1%20No%20queue%20is%20unbounded), [§2.2](rfc-24-resources.md#2.2%20What%20is%20outside%20the%20budget) | §2.2 and §5.1 list two rings and 20 MiB | add the `logon` ring, 4,096 entries, for three rings and 24 MiB |
| [RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits), [§19](rfc-22-smb.md#19.%20Observability) / [RFC 20 §10.1](rfc-20-adapters.md#10.1%20Metrics) / [RFC 17 §7](rfc-17-vfs.md#7.%20Observability) | `vfs_held_calls`, `vfs_held_bytes`, `vfs_hold_pool_bytes` and `vfs_hold_refusals_total` are defined nowhere; RFC 17 §7 and RFC 24 §10 say held requests are RFC 20 §10.1's, which lists none | RFC 20 §10.1 adds `adapter_held_requests{protocol}`, `adapter_held_bytes{protocol}` and `adapter_hold_refusals_total{protocol}`; the hold pool's fill is RFC 24's `memory_pool_reserved_bytes{pool="hold"}`; RFC 22 uses those names |
| [RFC 24 §5.1](rfc-24-resources.md#5.1%20No%20queue%20is%20unbounded), [§10](rfc-24-resources.md#10.%20Observability) | `work_items_dropped_total{source}`: the prefix `work_` has no owner, and the drop is decided by [RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler)'s scheduler | `engine_work_items_dropped_total{source}` in RFC 8 §14; RFC 24 links it |
| [RFC 24 §10](rfc-24-resources.md#10.%20Observability) | `memory_pool_ceiling_bytes{pool}` is a ceiling not named by §2.2's `_limit` rule | `memory_pool_reserved_bytes_limit{pool}` (§2.2) |
| [RFC 11 §13](rfc-11-ownership.md#13.%20Observability) | `shard_capacity_share` is a fraction without `_ratio` | `shard_capacity_ratio` |
| [RFC 13 §8](rfc-13-configuration.md#8.%20Observability) | `config_provisioning_applied`: a hash is no gauge value and no fixed info label | `config_provisioning_applied_timestamp_seconds`; the hash goes in the node-status record ([RFC 23 §2.4](rfc-23-control-plane.md#2.4%20What%20a%20node%20reports)) |
| [RFC 13 §8](rfc-13-configuration.md#8.%20Observability) | `format_supported_range`: one gauge cannot hold a range | `format_supported_min`, `format_supported_max` |
| [RFC 2 §2.3](rfc-2-carver.md#2.3%20What%20a%20pass%20makes%20observable) | `carver_chunk_size_bytes` buckets at a per-namespace `Target`, which one labelled vector cannot hold | the `size` set (§2.6); the average against `Target` is already `carver_bytes_total` ÷ `carver_chunks_total` |
| [RFC 26 §7](rfc-26-catalog-backups.md#7.%20Observability) | `backup_folder_*` are "per block folder", with the label unnamed | label `location` |
| [RFC 20 §10.1](rfc-20-adapters.md#10.1%20Metrics) | `adapter_requests_in_flight` spells what `vfs_inflight_calls` and `syncer_inflight_bytes` spell otherwise | `adapter_inflight_requests` (one rename instead of two) |
| [RFC 20 §10.2](rfc-20-adapters.md#10.2%20Spans) | span names `nfs4 COMPOUND`, `SEQUENCE` | `adapter.nfs4.COMPOUND`, `adapter.nfs4.SEQUENCE` (§6.1) |
| [RFC 4 §4.12](rfc-4-remote-tier.md#4.12%20What%20a%20store%20makes%20observable) | `remote_request_seconds` by `store` and 16 `op` values is 64 × 16 × 21 = 21,504 series, past §2.5's 10,000 | the histogram's `op` takes the five data calls (`put`, `get`, `delete`, `list`, `health`) and `other` for the rest: 64 × 6 × 21 = 8,064; `remote_requests_total` keeps every `op` |

Tables found to follow §2 as written: [RFC 14](rfc-14-open-state.md#14.%20Observability),
[RFC 15](rfc-15-topology.md#10.%20Observability) and
[RFC 18](rfc-18-identity.md#9.2%20Metrics), with their histograms to take
the `request` or `background` set of §2.6.

## 12. Conformance

The checks run at the tiers [RFC 29 §4.3](rfc-29-test-harness.md#4.3%20Tiers)
names, on the composed single-node harness of the [index](rfc-index.md#Test%20harness).

| Rule | Check | Catches |
| --- | --- | --- |
| §2.1, §2.2 names | Scrape a node that has run the reference workload. Assert every name's prefix is in §2.1's registry, every counter ends `_total`, every unit word is one of §2.2's. `metrics.golden` lists every name, type and label set; the scrape is diffed against it, and `-update` rewrites it. | a metric with a borrowed prefix, milliseconds; a quiet rename |
| §2.4 forbidden labels | On the same scrape assert no series carries a forbidden label, and no counter or histogram carries `share` or `shard`. Plant a share named like a file and a user named like a path: assert neither value appears in any label. | a share label slipped onto a histogram |
| §2.5 budget | Configure 10⁴ shares and every entity label at 64 values, run every protocol on one node, scrape. Assert at most 50,000 series and no metric above 10,000. Raise `metrics.top_shares` to 10⁴: assert the count rises by the per-share gauges only. | a label that scales with shares or clients |
| §2.3 zero start | Start a node, scrape before any traffic. Assert every closed-set series of every bug counter is present at 0. | an alert on "any increase" that misses the first event |
| §2.8 export does not block | Hold the engine's state lock; scrape. Assert the scrape returns within 6 s without the engine's series and `health_export_partial_total` rose; release; assert a write completed during the scrape. | an export that takes a request-path lock |
| §3.3 share does not set node | Deny puts on one store under one share of a hundred: assert the share unhealthy naming the store, the node's status unchanged by it, `health_shares{state="unhealthy"}` = 1, readiness and liveness true. | a readiness probe that restarts for a bucket policy |
| §3.4 never latched | Make a store get-unhealthy, then fix it without touching DittoFS. Assert the condition clears within its owner's bound and the share returns healthy, with no operator action. | a latched condition |
| §3.5 liveness | Stop the metadata store and the remote, fill the journal's device: assert liveness stays true throughout. Block the request scheduler: assert liveness fails within 60 s. | liveness on a dependency |
| §3.6 readiness | Assert not ready during recovery, ready in grace, ready while self-fenced, not ready with the metadata store lost and while draining. Drop one listener: assert not ready and `adapter_listener_lost`. Enable an adapter with a setting it cannot apply: assert ready, `adapter_refused` naming it, and the other adapter and the management API serving. Fail a start step: assert `not_ready` and `start_failed` naming the step. **(cluster)** Relinquish a shard on a node: assert the node stays ready and `shard_relinquished` is raised. | a node unreachable for reclaim; a probe stricter than §3.6 |
| §3.9 registry | §3.9's Go table and the RFC table agree, and every `Name` is returned by some `Source` in a test that injects its cause, with the severity the registry's rule gives that cause. | a condition no alert knows |
| §4.3 never blocks | With the access policy at every decision, stop the writer; run 10⁵ operations. Assert all 10⁵ operations complete, `events_dropped_total{reason="ring_full"}` equal to the events beyond the ring, and, once the writer resumes, one gap record covering exactly those sequence numbers, after every record below them and before every record above. Stop again, drop, and emit nothing more: assert the gap record is written within a second of the writer resuming. | a blocking sink; a silent drop; a gap out of order |
| §4.4 coalescing | Hammer a full quota with 10⁴ writes in a minute. Assert exactly two hard-refusal records: the first, at once, and one summary carrying the count of the rest. | a quota stream that floods |
| §4.5 restart | Kill the process with events in the ring. Restart: assert a `restart` gap record and a new boot ID, no sequence number reused within a stream and boot, and spool file names still sorting in write order across the restart. | a reused sequence after a crash |
| §4.6 retention | Fill the spool past `events.max_bytes`: assert the oldest file deleted, a `trimmed` gap record, `event_spool_high` raised first. Advance the clock past `events.retention` with the spool under its bound: assert the old files retired, no gap record and no `event_gap`. Put `events.path` on a journal's filesystem: assert the start refused. | a spool that eats journal capacity; a page for every hour of normal retention |
| §4.8 access | As `viewer` and as `operator`, read the access, logon and admin streams: refused. Change a share's audit policy: assert an `admin` record in the audit. Assert spool modes 0600/0700 and a start refused on a world-readable path. | audit data readable by any account |
| §5.4 never logged | Configure every secret-bearing record, plant known file names and known xattr contents, run the full suite at `Info`, and again at `Debug` for the second check. Search every line, span, health answer, metric and record: assert no secret anywhere, no content anywhere, no planted name at `Info` or above or outside the access stream. | a path in a warning; a token in a span |
| §5.2 per-request lines | Run 10⁵ operations of every kind with clients provoking every expected error. Assert the count of lines at `Info` and above does not grow with the operation count. | a log line per request |
| §5.2 debug expiry | Turn debug on for one component; advance the clock 15 minutes: assert it is off. Set `Debug` in configuration: assert it stays and `debug_logging_on` is raised. | debug left on; a developer's debug silently dropped |
| §6.2 propagation **(cluster)** | Forward a write from P1 to S2 with sampling at 1: assert one trace with P1's span parenting S2's service span. Retry the write: assert the same request ID, a new span, and one application. | trace context in the dedup key |
| §6.3 cost | `testing.AllocsPerRun` of starting and ending an unsampled span is 0, and of each `Sink` method is 0. | a span that allocates unsampled; a sink that boxes its event |

## 13. Open questions

1. **A per-share counter profile.** §2.4 forbids share labels on counters. An
   installation with tens of shares might want them; whether to offer a setting,
   and what its ceiling would be, waits for such a deployment.
2. **Tamper evidence on the node.** §4.5 relies on the shipper for immutability.
   Whether a regulated profile needs a chained digest per spool file, and whether
   its key is a namespace key or a node key, is open.
3. **Failing closed on audit.** §4.4's decision names a share setting that would
   refuse operations while the access ring is full. Whether any customer needs it
   is unknown.
4. **An auditor apart from the administrator.** §4.8 follows RFC 23's roles, in
   which only `admin` reads the audit, so whoever administers the installation can
   read who opened which file. Separation of duties wants an auditor who can read
   the streams and change nothing, and an administrator who cannot read them;
   whether to add that role is [RFC 23](rfc-23-control-plane.md#5.2%20Roles)'s.
5. **Condition delays.** The registry's 5- and 15-minute delays are starting
   values; the first months of alert history should replace them, as a
   proposed default is replaced by its measurement.
6. **Where the `request` set starts.** A KV call, a journal-hit read or a cached
   attribute read may all land in the first, 100 µs, bucket. Starting at 25 µs
   (proposed) would resolve them but costs two boundaries near the top, where
   pacing waits sit. A profile of the reference box settles it.
7. **A bound on the node-status record.** §3.8 writes a node's conditions, and
   its primary shares', into one record at most once a second; a share's
   `file_offload_failing` names FileIDs. A cap — 256 conditions and 8 FileIDs per
   condition, each with the count of the rest, proposed — must keep exact counts
   per severity so the installation status (§3.3) stays right; the numbers wait
   for a measured record size at 10⁴ shares.

## Appendix A — prior art

*Non-normative.*

- **Prometheus and OpenMetrics naming** — base units, `_total` on counters, unit
  suffixes, and the warning against unbounded label values: the source of §2.1–§2.4.
  OpenMetrics' state set and info types are §2.3's.
- **OpenTelemetry semantic conventions and the W3C Trace Context** — span naming,
  attribute names, and the `traceparent` encoding §6.2 means by "the standard
  trace-context encoding"; its exponential histograms are §2.6's upgrade.
- **Ceph's health checks** — named checks with a severity (`HEALTH_WARN`,
  `HEALTH_ERR`), and the cluster status as the worst of them: the model for
  §3.1–§3.4.
- **Kubernetes probes** — the advice that liveness must not depend on
  dependencies, since a restart loop amplifies an outage, and readiness that
  removes a pod from service endpoints: §3.5–§3.6.
- **Site reliability practice** — alert on symptoms, page only for what needs a
  person now, ticket the rest: §7.
- **Windows object-access auditing (SACLs and the Security log), Linux `auditd`,
  and NAS file-policy servers** — audit selected per object and per policy,
  events delivered to an external consumer, and, in some, the choice between
  dropping and blocking when the consumer falls behind: §4.

## Appendix B — where the current code differs

*Non-normative.* The code on the branch this RFC was written against.

| § | This RFC | The code today |
| --- | --- | --- |
| §2.1 | `dittofs_` plus a component prefix per owning RFC | `dittofs_` plus a subsystem — `adapter`, `auth`, `localstore`, `gc`, `snapshot`, `datapath`, `quota`, `client` — that follows packages, not the RFC components (`pkg/metrics/instruments.go`, `pkg/metrics/collectors.go`) |
| §2.4, §2.5 | no principal label; per-share gauges for the worst 20 | the read-through collector exports every share's gauges labelled `share`, and quota gauges labelled `scope, principal, share`, for every principal (`pkg/metrics/collectors.go`) |
| §2.6 | four fixed bucket sets, the request set ending at 60 s | most histograms use the client library's default buckets, 5 ms to 10 s, below the 30 s request deadline; the backpressure wait has explicit buckets to 90 s, with the reasoning in a comment (`pkg/metrics/instruments.go`) |
| §2.7, RFC 17 §7 | operation latency measured once, at the service | `adapter_request_duration_seconds{protocol, op}` and `adapter_requests_total{protocol, op, status}` are measured in the adapters; active connections are derived as accepted minus closed |
| §2.8 | metrics on a listener of their own | already so: opt-in, port 9090, optional bearer token and TLS (`pkg/metrics/server.go`, `pkg/config/metrics.go`) |
| §3.1–§3.4 | named conditions, a registry, derived status | five statuses — healthy, degraded, unhealthy, unknown, disabled — with a cached checker per entity and a free-form message; no condition names, no registry, no roll-up (`pkg/health`) |
| §3.5 | liveness reads no dependency | the liveness answer probes the control-plane database (cached 5 s) and reports degraded when it is unreachable, always with HTTP 200 (`internal/controlplane/api/handlers/health.go`) |
| §3.6, §3.7 | readiness false only while starting, draining, with the store or a listener lost, or after a failed start step; no detail without credentials | readiness answers 200 whenever the runtime exists, and reports share and store counts, running adapters and NFS grace state, unauthenticated (`internal/controlplane/api/handlers/health.go`, `pkg/controlplane/api/router.go`) |
| §4 | three spooled event streams | no access, quota or administrative event stream exists; no event sink type |
| §5.3 | trace ID on every request-scoped line | the logger defines `trace_id` and `span_id` keys but nothing sets them, and its context-taking functions ignore the context (`internal/logger/fields.go`, `internal/logger/logger.go`); the management API logs a request ID from its router middleware |
| §5.4 | no user-chosen name at `Info` or above | SMB handlers log paths and names at `Warn` on write, commit and create failures (`internal/adapter/smb/handlers/write.go`, `create_complete.go`); secrets are redacted in API answers only (`internal/controlplane/api/handlers/helpers.go`), not by the logger |
| §5.2 | debug turns itself off | the level is set from configuration or the environment and stays (`internal/logger/logger.go`) |
| §6 | spans per request and per service call | no tracing; the tracing library is only an indirect dependency (`go.mod`) |
| — | troubleshooting by condition | the troubleshooting guide's monitoring advice is to enable the metrics endpoint and grep the log for "slow" or "timeout" (`docs/guide/troubleshooting.md`) |
