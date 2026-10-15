---
rfc: 25
title: "RFC 25 — observability"
component: observability
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-18-identity]]"
  - "[[rfc-19-authorization]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-23-control-plane]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-29-test-harness]]"
  - "[[rfc-30-kubernetes]]"
aliases:
  - RFC 25
  - observability
tags:
  - rfc
---
# RFC 25 — observability

**Status:** draft. [§12](#12.%20Open%20questions) lists what is known to be undecided.
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
  ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)) are queued, stored, kept and
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
  node: access events, quota events, administrative events (§4).
- **spool** — the node-local, bounded, append-only files a stream is written to
  before it is exported (§4.3).
- **gap record** — a record a stream writes in place of events it had to drop, so
  a consumer knows the stream is incomplete and where (§4.4).
- **entity label** — a label whose values are configured objects (a store, a
  namespace, a journal, a pool), allowed but capped (§2.4).
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
  appear only in the debug level, which turns itself off.

**How the rest is organised.** §1 is purpose and boundaries. §2 is the metric
conventions, §3 health derivation, §4 the event streams, §5 logs, §6 tracing and
§7 alerting. §8 holds the invariants, §9 this RFC's own metrics, and §10 the audit
of every other RFC's table against §2, as edits. §11 is conformance and §12 open
questions. Appendix A is prior art; Appendix B where the current code differs.

### Scenarios

**bob is refused an open on `profiles/alice`, and the audit consumer is slow.**

1. The share's audit policy selects denials. bob-pc's `CREATE` reaches the
   chokepoint, which refuses it; the service emits an `AccessEvent` with
   `Allowed` false ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks),
   [RFC 19 §3.7](rfc-19-authorization.md#3.7%20Audit%20entries%20select%20events)).
2. The event goes into the access stream's in-memory ring and the call returns at
   once (§4.3). The writer appends it to the spool within a second.
3. The external shipper that tails the spool has been down for a day. The spool
   reaches its byte bound; the oldest file is deleted. The node raises
   `event_spool_trimmed` and writes a gap record naming the range of sequence
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

**A node restarts and enters grace.**

1. While its journals recover the node is live but not ready (§3.6).
2. Once its adapters accept clients it is ready, though in grace: clients must
   reach it to reclaim their state, so grace is a reason to be ready, not one to
   refuse ([RFC 30 §4.4](rfc-30-kubernetes.md#4.4%20Health)).
3. The grace period appears in the health report as information, not as a
   condition, and pages no one.

Outcome: clients reclaim; nothing alerts on a restart behaving as designed.

**A write from alice-pc reaches P1 and runs on S2.**

1. P1's adapter opens the request span; the service call is its child (§6.1).
2. The service forwards through the shard service. The route envelope carries the
   trace context beside the request ID, never inside it
   ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope), §6.2).
3. On S2 the service's span is a child of P1's, and parents the engine's and the
   metadata store's spans.

Outcome: one trace shows both nodes, and a retried write keeps its request ID
whatever the tracing says.

## In short

- Metric names are `<component>_<what>_<unit>`, counters end `_total`, units are
  seconds and bytes, and every prefix has one owning RFC.
- No share, shard, client, principal, file or connection label on any counter or
  histogram. Configured objects — stores, namespaces, journals, pools — may label
  anything, capped at 64 values per metric. Per-share gauges are exported for the
  worst 20 shares and summed; every share's are read through the management API.
- A node exports at most 50,000 series at the reference profile and no metric more
  than 10,000; the conformance run counts them.
- Histograms use one of four fixed bucket sets; summaries are not used.
- Health is the worst severity among current conditions in scope. Store and
  namespace conditions flow to their shares; shares do not make a node unhealthy.
- Liveness fails only on a stalled process. Readiness fails only while the node
  can serve no share at all: starting, draining, or with its metadata store lost.
- Three event streams — access, quota, administrative — are written to
  node-local spool files and exported as JSON lines. Access and quota events pass
  through a fixed ring: a drop never blocks an operation and always leaves a gap
  record. Administrative records are read from the control plane's durable audit.
- Logs: constant messages, fixed keys, a trace ID on every request-scoped line;
  no secrets, contents or file names above debug; debug turns itself off.
- One span per service call, parented across nodes through the route envelope;
  head sampling, off unless an export endpoint is set.
- Pages are for clients failing, data at risk, or a state only an operator can
  leave; everything else is a ticket.

---

## 1. Purpose

A component's metrics, conditions and log lines are written by whoever writes the
component, one RFC at a time. Without shared rules they grow apart: one table
counts in milliseconds and another in seconds, one labels by share and melts the
monitoring system, two tables count the same event under two names, and a
readiness probe built on "all components healthy" restarts a node for a fault the
restart cannot fix. This RFC fixes the rules once, and owns the parts no
component can own: how health combines, and how the event streams leave the node.

### 1.1 Non-goals

This RFC **MUST NOT** be read as specifying:

- any component's own metrics, conditions or log lines: each is its owner's,
  listed in its Observability section. This RFC audits those tables (§10) and does
  not restate them;
- the management API: which endpoints exist, their paths and their
  authentication are [RFC 23](rfc-23-control-plane.md)'s. This RFC fixes what the
  health, readiness and liveness answers report and how they are derived (§3.7),
  and what reading an event stream requires (§4.8);
- which operations produce an access event — the audit policy is a share setting
  ([RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)),
  applied by [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) and
  [RFC 19 §3.7](rfc-19-authorization.md#3.7%20Audit%20entries%20select%20events);
- what an access or quota event carries: [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)
  fixes that. This RFC adds only what the sink stamps on (§4.2);
- a monitoring, alerting, log-shipping or trace backend. DittoFS exports; it does
  not store metrics history, route alerts or archive audit records;
- profiling, which [RFC 16 §7.3](rfc-16-metadata-store.md#7.3%20Profiling) and the
  benchmark rules of the [index](rfc-index.md#Test%20tiers) cover.

### 1.2 Who owns what

| Topic | Owner |
| --- | --- |
| a component's metrics, conditions, log lines | that component's RFC, following §2–§5 |
| metric naming, units, types, labels, cardinality, buckets | this RFC, §2 |
| the condition registry: names, scope, severity, alert class | this RFC, §3.9; each new condition adds a row |
| how conditions combine into share, node and installation status | this RFC, §3.3 |
| what liveness, readiness and health answers report | this RFC, §3.5–§3.7 |
| the endpoints that serve them, and who may call them | [RFC 23](rfc-23-control-plane.md) |
| the Kubernetes probes and the cluster resource's conditions | [RFC 30 §4.4](rfc-30-kubernetes.md#4.4%20Health), reading §3.5–§3.6 |
| where access and quota events are emitted, and their fields | [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) |
| which decisions an audit entry selects | [RFC 19 §3.7](rfc-19-authorization.md#3.7%20Audit%20entries%20select%20events) |
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
| `api_` | [RFC 23](rfc-23-control-plane.md) |
| `api_`, `runtime_`, `audit_` | [RFC 23](rfc-23-control-plane.md#11.%20Observability) |
| `memory_`, `cpu_`, `fd_` | [RFC 24](rfc-24-resources.md) |
| `health_`, `events_`, `build_` | this RFC (§9) |
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
- A value that is a setting's current value is exported under the measured
  metric's name with `_limit` (`memory_pool_ceiling_bytes` aside, whose RFC names
  it), so the two plot on one axis.

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
- **Every series whose label values are a closed set is exported from start, at
  zero.** A counter that first appears at 1 is invisible to a rate, so an alert
  written as "any increase" would miss the first event — the one that matters for
  every counter whose owner says "any nonzero value is an alert".

### 2.4 Labels

A label's values **MUST** be a set whose size is known before deployment. There
are three kinds of allowed label, and a list of forbidden ones.

1. **Closed sets**, named in the owning RFC's table: `op`, `protocol`, `outcome`,
   `reason`, `result`, `kind`, `direction`, `state`, `role`, `cause`. Values are
   lower-case words; an error label takes the names of the closed error set it
   reports ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values),
   [RFC 4 §4.8](rfc-4-remote-tier.md#4.8%20Errors%20are%20a%20closed%20set)). The
   `protocol` label takes `nfs3`, `nfs4` or `smb` — minor versions are not
   separate values — and the side programs count under `nfs3`.
2. **Entity labels**, whose values are configured objects: `store`, `namespace`,
   `journal`, `pool`, `location`, `transform`, `node`. Any metric may carry them.
   Each metric exports at most **64** values of each entity label per node; past
   64 the rest are summed into the value `_other`, and the node raises
   `metrics_label_capped` naming the metric (§3.9). An entity label takes the
   object's name, not an internal ID, so a dashboard reads.
3. **`share` and `shard`, on gauges only**, under the worst-n rule of §2.5.

**Forbidden on every metric:** `client`, `principal`, `user`, `group`, `file`,
`path`, `name`, `session`, `connection`, `request`, `handle`, any key, material or
snapshot ID, and any value a client or a user chooses. They are unbounded, and
some are sensitive. Who did something is an event (§4); which file, a log line or
a span attribute (§5, §6).

**No share or shard label on any counter or histogram.** This generalises
[RFC 16 §8.1](rfc-16-metadata-store.md#8.1%20Metrics): at 10⁴ shares, a share
label on `vfs_op_seconds` multiplies its 8,000 series by 10⁴. A per-share count of
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

**Series budget.** At the reference profile — 10⁴ shares, every protocol in use,
every role on one node — a node **MUST** export no more than **50,000** series in
all and no single metric more than **10,000**, histogram buckets included. Both
are fixed design limits, checked by counting after the reference workload (§11);
a table whose metric cannot fit is a defect in that table. `vfs_op_seconds` sets
the scale: about 45 operations × 3 protocols × 3 outcomes × 20 series per
histogram is 8,100.

> ponytail: the budget is checked by one count at one profile. Past it, add a
> per-metric series limit in the exporter that drops and counts new series,
> when a deployment shows a metric growing in a way the profile did not exercise.

![The operation-latency histogram costs about 8,000 series with op, protocol and outcome; a share label would multiply that by 10,000 to about 80 million; per-share figures are gauges exported for the worst 20 shares plus an unlabelled sum, and every share is read through the management API](img/rfc25-labels.svg)

### 2.6 Histogram buckets

A histogram uses one of four fixed bucket sets, so the same quantity aggregates
across metrics and nodes:

| Set | Boundaries | For |
| --- | --- | --- |
| `request` | 100 µs, 250 µs, 500 µs, 1 ms, 2.5 ms, 5 ms, 10 ms, 25 ms, 50 ms, 100 ms, 250 ms, 500 ms, 1 s, 2.5 s, 5 s, 10 s, 30 s, 60 s | anything a client request waits on: operations, KV calls, remote calls, recalls |
| `background` | 10 ms, 100 ms, 1 s, 5 s, 10 s, 30 s, 60 s, 5 min, 15 min, 1 h, 6 h | passes, handovers, takeovers, freezes, snapshot completion |
| `size` | 512 B and each power of four up to 64 MiB, then 256 MiB | payload, block and transaction sizes in bytes |
| `count` | 1, 2, 4 … 4,096 | keys per transaction, files per commit, refs per batch |

The `request` set ends past 30 s, the default request deadline
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)), and 35 s, the
SMB break deadline, so a request that runs out its deadline lands in a finite
bucket. A histogram whose owning RFC states boundaries tied to its settings —
the carver's chunk sizes at `Min`, `Target` and `Max`
([RFC 2 §2.3](rfc-2-carver.md#2.3%20What%20a%20pass%20makes%20observable)) — uses
those and says so.

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
misses what the other saw. §10 lists the duplicates in the set today.

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
- An export carries no file name, no user content and no secret (§2.4, §5.4). So
  it is not audit data; whether it is authenticated is still RFC 23's choice,
  since share and store names are.

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
- A component raises and clears its own conditions; nothing else may.
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
  counted in `health_condition_transitions_total{condition}` (§9).
- An operator may **silence** a condition's alerts for a stated time, through the
  management API. Silencing changes no status: the condition still shows, marked
  silenced, and the silence ends at its time.

### 3.5 Liveness

**Liveness answers one question: is the process making progress?** It **MUST**
fail only when a fixed set of internal loops — each adapter's accept loop, the
request scheduler, the engine's work scheduler and the health evaluator — has
not completed a turn, or reported itself idle, within 60 s. It **MUST NOT** read a
condition, a store, the network or the disk.

> decision: no dependency fails liveness. A liveness failure asks the
> orchestrator to restart the process, and no dependency failure in this set is
> fixed by a restart: the journal recovers, every NFS client waits out a grace
> period, every SMB client reconnects, and the store is as unreachable as before.
> Overturned by a failure a restart demonstrably clears, which is then a bug to
> fix, not a probe to add.

### 3.6 Readiness

**Readiness answers whether the node should be sent new clients.** It fails only
while sending them elsewhere would help: the node can serve no share at all.

| Node state | Live | Ready | Status |
| --- | --- | --- | --- |
| starting: opening stores, recovering journals, re-applying existence | yes | no | starting |
| serving | yes | yes | from its conditions |
| in grace after a restart | yes | yes — clients must reach it to reclaim | from its conditions; grace is information |
| self-fenced: no write transaction committed for 30 s ([RFC 8 §11.2](rfc-8-engine.md#11.2%20Every%20condition%20in%20RFC%200%20%C2%A710%20has%20its%20engine%20behaviour%20here)) | yes | yes — reads continue | unhealthy, `node_fenced` |
| some shares refused: a journal device lost, a share's store denied | yes | yes | unhealthy for those shares |
| started from a copied image | yes | yes — reads of offloaded content continue | unhealthy, `copied_image` |
| metadata store lost: no share served until a recovery import | yes | no | unhealthy, `metadata_store_lost` |
| draining: stop requested | yes | no | draining |
| watchdog stalled | no | no | — |

For each role ([RFC 15 §2.4](rfc-15-topology.md#2.4%20Composition%20by%20role)):

- a node with the `protocol` role is ready once its adapters accept clients and
  the share list is loaded;
- a node with the `storage` role is ready once it has joined the store and holds
  no shard it cannot serve **(cluster)** — the readiness
  [RFC 30 §4.4](rfc-30-kubernetes.md#4.4%20Health) gates a rolling upgrade on;
- a node with both roles is ready when both are.

### 3.7 What the health answers report

[RFC 23 §2.5](rfc-23-control-plane.md#2.5%20Health%20and%20readiness%20endpoints)
serves three answers. Their content is this:

- **liveness**: the one word `live` while §3.5's watchdog holds; a failed answer
  once it has not, and nothing else.
- **readiness**: one word, the node state from §3.6's table — `ready` while
  ready, otherwise `starting`, `draining`, or `not_ready` with the metadata store
  lost — and nothing else.
- Neither answer names a share, a store or a condition, so either can be served
  to a probe that holds no credential
  ([RFC 23 §2.5](rfc-23-control-plane.md#2.5%20Health%20and%20readiness%20endpoints)).
- **health**: the status of the installation, of this node and of each scope in
  it; every current condition with its fields (§3.1), whether it is silenced, and
  its alert class; the counts of shares by status and the worst 20 shares; and
  the information section — grace periods and their remaining time, snapshots and
  backups running, drains. With a share or store named, the same for that subject
  alone. It names shares and stores, so it needs the `viewer` role
  ([RFC 23 §5.2](rfc-23-control-plane.md#5.2%20Roles)).

### 3.8 Installation health (cluster)

**(cluster, [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile))**
On one node, the installation's health is the node's, computed in memory. With
more than one:

- each node writes its node-scope conditions, and those of the shares it is
  primary of, into its node-status record
  ([RFC 23 §2.4](rfc-23-control-plane.md#2.4%20What%20a%20node%20reports)), only
  when they change and at most once a second, so health costs the store a
  bounded write rate;
- any node answers the installation's health by reading every node record; a
  node whose lease has lapsed is the installation condition `node_lost` naming it,
  and its last written conditions are shown as stale;
- a share's conditions come from its primary's record; a protocol node that
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
| `offload_age_exceeded` | [RFC 8 §11.4](rfc-8-engine.md#11.4%20How%20far%20behind%20offload%20is%2C%20is%20observable) | share | degraded | ticket at once |
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
| `namespace_count_underflow` | [RFC 7 §11.2](rfc-7-namespace-metadata.md#11.2%20Observability) | share, naming the directory's FileID | degraded | ticket |
| `snapshot_policy_skipping` | [RFC 12 §6](rfc-12-snapshots.md#6.%20Observability) | share, at 3 skipped ticks | degraded | ticket |
| `backup_failing`, `backup_location_mismatch` | [RFC 26 §7](rfc-26-catalog-backups.md#7.%20Observability) | share; location | degraded | ticket |
| `directory_unreachable` | [RFC 18 §10.2](rfc-18-identity.md#10.2%20Metrics) | installation | degraded within the stale grace; unhealthy past it, since sign-ins are refused | ticket; page at 5 min when unhealthy |
| `memory_limit_below_floors`, `fd_limit_low`, `memory_near_limit` | [RFC 24 §10](rfc-24-resources.md#10.%20Observability) | node | degraded | ticket |
| `metrics_label_capped` | §2.4 | node | degraded | ticket |
| `event_spool_high` | §4.6 | node | degraded, above 80% of the spool bound | ticket |
| `event_gap` | §4.4 | node, naming the stream | degraded | page for the access stream while any share's audit policy is on; ticket otherwise |
| `replication_under_floor` **(cluster)** | [RFC 10 §14](rfc-10-journal-replication.md#14.%20Observability) | share | degraded | page at 5 min |
| `shard_relinquished` **(cluster)** | [RFC 10 §14](rfc-10-journal-replication.md#14.%20Observability) | share | unhealthy; waits for a replica or the operator | page at once |
| `node_lost` **(cluster)** | [RFC 11 §9](rfc-11-ownership.md#9.%20Failure), §3.8 | installation | degraded while its shards fail over | page at 5 min |

## 4. Event streams

### 4.1 Three streams

| Stream | Emitted by | Carries | Selected by |
| --- | --- | --- | --- |
| `access` | the filesystem service, where it consults the chokepoint ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)) | one authorisation decision | the share's audit policy and the file's audit entries ([RFC 19 §3.7](rfc-19-authorization.md#3.7%20Audit%20entries%20select%20events)) |
| `quota` | the filesystem service, where it enforces quota ([RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)) | a threshold crossed, a soft limit exceeded, a grace expired, a hard refusal, a return under the limit | always |
| `admin` | the control plane's audit records ([RFC 23 §6](rfc-23-control-plane.md#6.%20The%20audit)) | one management action or refusal: actor, target, change, outcome — a governance-lock override, a statement that a location is lost, a share's audit policy changed | always |

The administrative stream is the export of the management audit that other RFCs
record operator actions in ([RFC 13 §9](rfc-13-configuration.md#9.%20Test%20plan),
[RFC 12 §6](rfc-12-snapshots.md#6.%20Observability)). Unlike the other two it
starts from durable records: RFC 23 commits each audit record in the
transaction of the change it records, keeps it for `audit.retention`, and owns
its fields. This section owns only how those records leave the node (§4.3).

Alarm entries select access events like audit entries do
([RFC 19 §2.2](rfc-19-authorization.md#2.2%20Entry%20kinds)): a record says which
selected it, and this RFC defines no second consumer that would need them apart.

### 4.2 What a record carries

A record is one JSON object (RFC 8259) on one line. The event's own fields are
[RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)'s; the sink stamps on the rest:

| Field | Set by | Means |
| --- | --- | --- |
| `v` | sink | the record format's version, 1; a consumer refuses a version it does not know |
| `stream` | sink | `access`, `quota`, `admin` |
| `node`, `boot`, `seq` | sink | the node's ID, a random ID drawn at each process start, and a sequence number counted from 0 per stream and boot; `(node, boot, seq)` names a record uniquely and shows any gap |
| `time` | event | the event's time, RFC 3339 in UTC with nanoseconds |
| `trace_id` | sink, from the call's context | the trace the decision ran in (§6), when sampled |
| `share` | event | the share's ID and name |
| `client`, `protocol` | event | the client's ID and address, and the protocol |
| `identity` | event | the principal ID, the flavour and whether authenticated, as [RFC 18](rfc-18-identity.md) resolved them — not a uid alone, which two principals may share |
| `op`, `file`, `name` | event | the operation, the FileID, and the name acted on for a namespace operation |
| `want`, `allowed`, `selected_by` | event | the rights asked, the decision, and `policy` or `entry` |
| quota fields | event | scope (`user`, `group`, `project`, `share`), subject, resource (`bytes`, `files`), kind, used, soft, hard, grace end, and for a hard refusal the writer's identity and the count coalesced (§4.4) |

- A name is written as UTF-8 when it is valid UTF-8, and otherwise as base64 under
  `name_b64`, so no byte a client chose can break a line or a parser.
- **A record carries no file content and no secret.** It carries a file name only
  in the `access` stream and only for namespace operations, where the name is the
  thing decided.

[RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) shows the `AccessEvent` type and
lists what a quota event is for, but not its fields; §10 asks it to add
`QuotaEvent` with the fields above.

### 4.3 From the emit point to the spool

![The service calls the sink at the chokepoint and at quota enforcement and never waits; the sink stamps node, boot and sequence and puts the event in a fixed ring per stream, dropping and counting when full; one writer per stream appends batches to spool files every second, writing a gap record for anything dropped; the spool is bounded by age and bytes and is read by a shipper tailing the files or by an auditor through the management API](img/rfc25-events.svg)

1. **The service calls the sink and returns.** `Events.Access` and
   `Events.Quota` take a value and put it into the stream's ring; they **MUST NOT**
   allocate on the caller's path beyond the copy of the name, take a lock a
   request path also takes, or wait
   ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)).
2. **One ring per stream, of fixed size**: 16,384 entries for `access` and 4,096
   for `quota`, each entry at most 1 KiB. The rings are allocated at start, so
   they are fixed memory outside RFC 24's pools: at most 20 MiB per node,
   whatever the load.
3. **One writer per stream** takes entries in order and appends them to the
   stream's current spool file, in batches, syncing the file and making it visible
   every second.
4. **Spool files** are rotated at 64 MiB or one hour, whichever comes first, and
   named by stream, boot and first sequence number, so their names sort in order.
5. **The `admin` stream has no ring.** Each node's writer reads, in commit order,
   the audit records of requests that node took (their `node` field), and appends
   them to the stream's spool, keeping its position in a node-local cursor synced
   with each batch. A restart resumes at the cursor, so nothing is lost and a
   record may be written twice across a crash; a consumer deduplicates by the
   record's request ID. Each record is exported by one node, the one that took
   its request **(cluster)**.

### 4.4 Bounds, drops and gap records

- **When a ring is full, the new event is dropped**, counted in
  `events_dropped_total{stream, reason="ring_full"}`, and remembered as a range of
  lost sequence numbers. The next record the writer appends is preceded by a
  **gap record**: `stream`, `kind: "gap"`, the first and last sequence numbers
  lost, the count, the time range, and the reason. So a consumer finds the loss in
  the stream itself, not only in a metric it may not watch.
- The same holds for every other loss: `spool_full` when the writer cannot
  append, `trimmed` when retention deletes a file (§4.6), and at start, `restart`,
  a gap record with no count for the events of the last unsynced second before
  the previous process ended. The `admin` stream loses records only to
  `trimmed`: a full spool makes its writer wait, since its source is durable.
- A gap in the access stream raises `event_gap` naming the stream (§3.9), cleared
  an hour after the last gap.
- **Quota events are coalesced.** A transition — threshold crossed, soft limit
  exceeded, grace expired, back under — is emitted once per transition. Hard
  refusals are coalesced per (share, scope, subject, resource): the first is
  emitted at once, and the rest within the next 60 s are counted into one record
  at the end of the window. A writer hammering a full quota produces one record a
  minute, not one per write.

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
- **Order is per node and stream.** Within one `(node, boot)` sequence numbers
  rise with the order the sink accepted events, which is the order of the
  decisions on that node. Across nodes there is no order but `time`
  **(cluster)**: a consumer merges by time and names records by
  `(node, boot, seq)`.
- **(cluster)** A quota transition is seen by every primary that writes for the
  subject, so it may be emitted by more than one node within the clock-offset
  bound. A consumer that needs one record per transition deduplicates by (share,
  scope, subject, kind) within a minute.

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
- A spool file is deleted once it is older than `events.retention` (default 7
  days, proposed) or once the spool exceeds `events.max_bytes` (default 4 GiB per
  node, proposed), oldest first. Both are settings of
  [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings).
- Deleting a file writes a `trimmed` gap record. The node does not know what a
  shipper has read, so a deletion is a loss as far as the stream is concerned.
- `event_spool_high` is raised above 80% of `events.max_bytes` — the warning that
  comes before a trim.

> decision: the spool is a buffer for a shipper, not the archive. Keeping audit
> records for months is the downstream store's job, with its own access control
> and immutability; a node that kept them would need both, and a quota on a disk
> it does not own. Overturned by a deployment with no log pipeline that must still
> keep audit records — then by a backup-location export of the spool, not by a
> larger spool.

### 4.7 Export

The spool files are the export: JSON lines, one record per line, rotated and named
in order (§4.3). A shipper tails them; the format needs no DittoFS code to read.
The management API also serves a stream's records, filtered by share, time and
sequence range, read from the same files ([RFC 23](rfc-23-control-plane.md) owns
the endpoint).

Events **MUST NOT** be exported as metrics or written to the log: the counters of
§9 count them, but no metric label or log line carries a record's identity, file
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
- **Reading the `access` and `admin` streams through the management API requires
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
- `Debug` may be turned on for one component at a time through the management
  API, and turns itself off after 15 minutes unless renewed. A debug level set in
  the configuration file holds for the first hour after start only.

> decision: debug expires. Debug lines carry file names (§5.4) and cost a line per
> request; one left on after an incident fills the disk and the log store with
> user content. Overturned by nothing likely: a longer investigation renews it.

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
  always be joined to its request even when no span was exported.
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
  at `Debug` only, and in the `access` stream (§4.2).

Share, store and namespace names, principal IDs, client addresses and client IDs
are operational identifiers and may be logged at any level.

The check is a test, not a review (§11): a full conformance run with known
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
- Background work — an offload pass, a GC pass, a handover, a snapshot, a backup —
  opens a root span of its own. A pass that serves a request it was waiting on, a
  fetch joined by a read, *links* to that request's span rather than becoming its
  child, so a request's trace shows the wait without absorbing the pass.
- Span names are constant: the operation or the stage. Attributes are the
  operation's closed-set fields, sizes, the share and file by ID, the outcome and
  the error. §5.4 applies to every attribute.

### 6.2 Propagation across nodes

- **(cluster)** The route envelope ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope))
  carries a **trace context** — the trace ID, the parent span ID and the sampled
  flag, in the standard trace-context encoding — beside the request ID. The
  primary's service span is the child of the forwarding node's. The trace context
  **MUST NOT** enter the request ID, the dedup key or any durable record: a retry
  carries a new span and the same request ID.
- A client's own trace context is not read from the wire; NFS and SMB carry none.
  A trace starts at the adapter.
- Replication and node-channel calls **(cluster)** carry the context of the work
  that caused them.

### 6.3 Sampling and cost

- Tracing is off unless `tracing.endpoint` names a collector (a node setting of
  [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)).
  With it set, requests are sampled at the adapter at `tracing.sample_ratio`
  (default 0.01, proposed), and the decision travels with the context, so a trace
  is whole or absent.
- An unsampled span **MUST** cost no allocation and no clock read beyond the trace
  ID itself, so tracing off costs the request path nothing measurable. The
  conformance run counts allocations per operation with tracing off and on at
  ratio 0 (§11).
- Spans are exported in batches by a writer of their own, from a bounded queue
  that drops and counts (`health_spans_dropped_total`, §9) rather than block.

> ponytail: head sampling only. A slow or failed request is kept only if it was
> sampled. Add tail sampling — keep every trace that ends in an error or past the
> p99 — in the collector, which can see the whole trace; DittoFS changes nothing
> for it.

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
- **nothing** — information: grace, drains, snapshots running, conditions
  silenced.

Each condition's class and delay are in the registry (§3.9). An alert names the
condition, the subject, the cause and `since`, and links the troubleshooting
guide's entry for the condition. One condition is one alert, however many shares
inherit it: `store_put_unhealthy` on a store under 300 shares is one page naming
the store and the count of shares, not 300.

Alerts that read metrics directly are few and named here: a node that has stopped
exporting (the scraper's own), `health_conditions` at the installation scope as a
backstop for an alert route that failed, and the bug counters above.

## 8. Invariants

| # | Invariant |
| --- | --- |
| O1 | Every metric's prefix has one owning RFC, its unit is in its name, and counters end `_total`. |
| O2 | No counter or histogram carries a share, shard, client, principal, file, path or connection label; no metric carries a value a client chose. |
| O3 | Every entity label is capped at 64 values per metric per node; per-share and per-shard gauges are exported for the worst `metrics.top_shares` and summed. |
| O4 | A node exports at most 50,000 series at the reference profile, no metric more than 10,000. |
| O5 | Every event is counted by one metric, in one RFC. |
| O6 | A status is the worst severity among current conditions in scope; a share's conditions never set its node's status. |
| O7 | Every condition is derived from current evidence and clears on its own, except the four states RFC 0 §10.2 names. |
| O8 | Liveness reads no condition, store, network or disk. |
| O9 | Readiness is false only while starting, draining, or with the metadata store lost. |
| O10 | An event hook never blocks or fails an operation; every event dropped leaves a gap record in its stream and a count. |
| O11 | Every record names its node, boot and sequence; within one boot sequence numbers have no gap that is not a gap record. |
| O12 | The spool is never on a journal's filesystem and is readable only by the service's account. |
| O13 | The access and admin streams are readable only with the right to read the audit, and never appear in a log, a metric or a health answer. |
| O14 | No log line, span, metric, health detail or record carries a secret or file content; no line at `Info` or above carries a user-chosen name. |
| O15 | No line at `Info` or above is written per client request. |
| O16 | The trace context never enters a request ID, dedup key or durable record. |

## 9. Observability

What this RFC itself exports. Metric names are shown without the prefix.

| Answers | Metric | Type |
| --- | --- | --- |
| this node's status and the installation's, as a state set | `health_status{scope, state}` | gauge |
| current conditions, by name and severity | `health_conditions{condition, severity}` | gauge |
| shares by status, on this node | `health_shares{state}` | gauge |
| condition raises and clears | `health_condition_transitions_total{condition, transition}` | counter |
| exports that omitted a component (§2.8) | `health_export_partial_total{component}` | counter |
| series exported, and entity labels capped (§2.4) | `health_series`, `health_label_capped_total{metric}` | gauge, counter |
| spans dropped by the export queue (§6.3) | `health_spans_dropped_total` | counter |
| records accepted, by stream | `events_records_total{stream}` | counter |
| records dropped, by stream and reason (`ring_full`, `spool_full`, `trimmed`) | `events_dropped_total{stream, reason}` | counter |
| ring occupancy against its size | `events_ring_entries{stream}`, `events_ring_entries_limit{stream}` | gauge, gauge |
| spool bytes against its bound, and the oldest file's age | `events_spool_bytes`, `events_spool_bytes_limit`, `events_spool_oldest_seconds` | gauge, gauge, gauge |
| time from accept to synced in the spool | `events_write_seconds{stream}` | histogram (`request`) |
| build and roles | `build_info{version, commit}` | info |

`condition` takes the registry's names, a closed set; `health_conditions` is
exported at zero for every registered condition (§2.3). `events_dropped_total`
is the one count of lost events: it replaces the drop counters of RFC 7 and
RFC 17 (§10).

## 10. Audit of the set's tables

Every Observability section was read against §2. Each row is an edit for the
owning RFC; none is applied here.

| RFC | Finding | Edit |
| --- | --- | --- |
| [RFC 1 §3.9](rfc-1-journal.md#3.9%20Metrics) | counters are named as `Stats` fields with no prefix or `_total`; per-share fields are labelled by share with no worst-n rule | "The engine exports each counter as `journal_<field>_total` — `written_bytes` as `journal_written_bytes_total`, `syncs` as `journal_syncs_total` — and each gauge as `journal_<field>`, labelled with the journal; per-share fields follow RFC 25's worst-n rule." |
| [RFC 3 §2.12](rfc-3-syncer.md#2.12%20What%20the%20syncer%20makes%20observable) | `syncer_store_throughput_bytes` is a rate named as a size; "queue metrics are also reported per flow", which the engine labels by share, includes the histogram `syncer_queue_wait_seconds` and the counter `syncer_throttled_seconds_total` | rename to `syncer_store_throughput_bytes_per_second`; "Queue gauges (`syncer_queue_depth`, `syncer_waiters`) are also reported per flow, which the engine exports by share under RFC 25's worst-n rule; queue histograms and counters are reported summed only." |
| [RFC 5 §6](rfc-5-transforms.md#6.%20Observability), Appendix B.6 | `encryption_chunks_total` is labelled by data key ID and `transform_census_blocks` by material ID, both growing with every rotation; `encryption_key_available` is one series per key | drop the key label from `encryption_chunks_total`; label `transform_census_blocks` by transform and version only, the per-material census being read through the management API; replace `encryption_key_available` with `encryption_keys_unavailable`, a count, with `material_unavailable` naming the keys |
| [RFC 6 §10.2](rfc-6-block-metadata.md#10.2%20Observability) | `blockmeta_history_refs` "per share" states no rule | "per share, under RFC 25's worst-n rule" |
| [RFC 7 §11.2](rfc-7-namespace-metadata.md#11.2%20Observability), §7.6 | `namespace_events_dropped_total` counts the drop RFC 17 also counts, at a sink neither owns; §7.6 still calls delivery "a planned RFC's" | remove the row; "events dropped are counted at the sink (RFC 25)". In §7.6: "are RFC 25's" |
| [RFC 8 §14](rfc-8-engine.md#14.%20Observability) | "a stated rule selects (the worst *n* by each figure)" points nowhere; the self-fence the engine owns has no counter, while RFC 10 and RFC 11 each count one | "under RFC 25's worst-n rule"; add `engine_self_fences_total{reason}`, `reason` = `store`, and **(cluster)** `lease`, `unreachable`, `clock`, the one count of self-fences |
| [RFC 9 §10](rfc-9-gc.md#10.%20Observability) | `gc_audit_coverage_age_seconds` is "per hash range", a label of unstated size | export the oldest range's age per namespace only |
| [RFC 10 §14](rfc-10-journal-replication.md#14.%20Observability) | "every metric is labelled by shard … and by share", histograms and counters included; `journal_applied_bytes_total` uses RFC 1's prefix; `node_self_fences_total` duplicates RFC 11's; `node_` has no owner | "Gauges that are per shard are exported under RFC 25's worst-n rule; no histogram or counter carries a shard or share label"; rename to `replication_applied_bytes_total`; remove `node_self_fences_total` in favour of RFC 8's; rename `node_clock_offset_seconds` to `replication_clock_offset_seconds` |
| [RFC 11 §13](rfc-11-ownership.md#13.%20Observability) | `shard_self_fences_total` duplicates the self-fence count | remove; link RFC 8's `engine_self_fences_total` |
| [RFC 12 §6](rfc-12-snapshots.md#6.%20Observability) | `snapshot_gate_seconds` is "per shard" on a histogram; `snapshot_lock_overrides_total` is "labelled by principal"; per-share gauges state no rule; "any value is an audit event" names no stream | drop the shard label; drop the principal label — the override is an `admin` record (RFC 25 §4.1); "per share, under RFC 25's worst-n rule" on `snapshot_held_bytes` and `snapshot_history_bytes` |
| [RFC 13 §8](rfc-13-configuration.md#8.%20Observability) | `config_generation` is labelled by record — one per share — and `config_secret_version` by reference — one per user secret | replace `config_generation` with RFC 23's `runtime_applied_generation{kind}`, read against the store's current generation per kind, and add `config_generation_lag_seconds{kind}`; replace `config_secret_version` with `config_secrets_behind`, a count; the `config_generation_behind` condition names the records |
| [RFC 16 §8](rfc-16-metadata-store.md#8.%20Observability) | `metadata_txn_size{kind=entries\|bytes}` and `…_txn_limit` select a unit by label; "On fold, they follow RFC 25's conventions; until then they start `metadata_`" is stale | split into `metadata_txn_entries`, `metadata_txn_bytes` histograms and `metadata_txn_entries_limit`, `metadata_txn_bytes_limit` gauges; "Metric names follow RFC 25's conventions, under the prefix `metadata_`." |
| [RFC 17 §7](rfc-17-vfs.md#7.%20Observability), §3.3 | `outcome` on `vfs_op_seconds` has no stated values, and the full error set would cost about 70,000 series; `vfs_events_dropped_total` is the sink's; §3.3 defines no `QuotaEvent`, and says "planned observability RFC" | "`outcome` = `ok`, `error` or `delay`; the error itself is counted in `vfs_errors_total{op, error}`"; remove the drop row, "events dropped are RFC 25's `events_dropped_total`"; add a `QuotaEvent` type with RFC 25 §4.2's quota fields; "belong to RFC 25" |
| [RFC 19 §1.1](rfc-19-authorization.md#1.1%20Non-goals), §10 | "the planned RFC 25's"; "events dropped are RFC 7's" | link [RFC 25 §4](rfc-25-observability.md#4.%20Event%20streams); "events dropped are RFC 25's" |
| [RFC 24 §5.1](rfc-24-resources.md#5.1%20No%20queue%20is%20unbounded), §2.2 | the events row's bound is "the stream's fixed buffer"; §2.2 does not list the rings as outside the pools | "access-audit and quota events \| rings of 16,384 and 4,096 entries of at most 1 KiB, allocated at start \| RFC 25 §4.4 \| dropped, counted, a gap record written"; add the event rings to §2.2's list |
| [RFC 26 §7](rfc-26-catalog-backups.md#7.%20Observability) | `backup_newest_age_seconds` "per share" states no rule | "per share, under RFC 25's worst-n rule, with the oldest unlabelled" |
| [RFC 27 §6](rfc-27-namespace-migration.md#6.%20Observability) | `namespace_owned` and `namespace_claim_copy_total` use RFC 7's prefix, `gc_claim_refusals_total` RFC 9's; `rehome_old_refs` per share states no rule | rename to `claim_owned`, `claim_copy_total` and `claim_gc_refusals_total`; worst-n for `rehome_old_refs` |
| [RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope) | the envelope has no trace context | add a row: "trace context \| the sender's trace ID, parent span ID and sampled flag ([RFC 25 §6.2](rfc-25-observability.md#6.2%20Propagation%20across%20nodes)); never part of the request ID" |

Tables found to follow §2 as written: [RFC 2](rfc-2-carver.md#2.3%20What%20a%20pass%20makes%20observable),
[RFC 4](rfc-4-remote-tier.md#4.12%20What%20a%20store%20makes%20observable),
[RFC 14](rfc-14-open-state.md#14.%20Observability),
[RFC 15](rfc-15-topology.md#10.%20Observability),
[RFC 18](rfc-18-identity.md#10.2%20Metrics),
[RFC 20](rfc-20-adapters.md#10.%20Observability),
[RFC 21](rfc-21-nfs.md#13.%20Observability),
[RFC 22](rfc-22-smb.md#18.%20Observability) and
[RFC 24](rfc-24-resources.md#10.%20Observability), with their histograms to take
the `request` or `background` set of §2.6.

## 11. Conformance

The checks run at the tiers [RFC 29 §4.3](rfc-29-test-harness.md#4.3%20Tiers)
names, on the composed single-node harness of the [index](rfc-index.md#Test%20harness).

| Rule | Check | Catches |
| --- | --- | --- |
| §2.1, §2.2 names | Scrape a node that has run the reference workload. Assert every name's prefix is in §2.1's registry, every counter ends `_total`, every unit word is one of §2.2's. | a metric with a borrowed prefix, milliseconds |
| §2.4 forbidden labels | On the same scrape assert no series carries a forbidden label, and no counter or histogram carries `share` or `shard`. Plant a share named like a file and a user named like a path: assert neither value appears in any label. | a share label slipped onto a histogram |
| §2.5 budget | Configure 10⁴ shares, run every protocol and role on one node, scrape. Assert at most 50,000 series and no metric above 10,000. Raise `metrics.top_shares` to 10⁴: assert the count rises by the per-share gauges only. | a label that scales with shares or clients |
| §2.3 zero start | Start a node, scrape before any traffic. Assert every closed-set series of every bug counter is present at 0. | an alert on "any increase" that misses the first event |
| §2.8 export does not block | Hold the engine's state lock; scrape. Assert the scrape returns within 6 s without the engine's series and `health_export_partial_total` rose; release; assert a write completed during the scrape. | an export that takes a request-path lock |
| §3.3 share does not set node | Deny puts on one store under one share of a hundred: assert the share unhealthy naming the store, the node's status unchanged by it, `health_shares{state="unhealthy"}` = 1, readiness and liveness true. | a readiness probe that restarts for a bucket policy |
| §3.4 never latched | Make a store get-unhealthy, then fix it without touching DittoFS. Assert the condition clears within its owner's bound and the share returns healthy, with no operator action. | a latched condition |
| §3.5 liveness | Stop the metadata store and the remote, fill the journal's device: assert liveness stays true throughout. Block the request scheduler: assert liveness fails within 60 s. | liveness on a dependency |
| §3.6 readiness | Assert not ready during recovery, ready in grace, ready while self-fenced, not ready with the metadata store lost and while draining. | a node unreachable for reclaim |
| §3.9 registry | For every condition the suite can raise, assert its name is in the registry and its severity matches the registry's rule for the cause injected. | a condition no alert knows |
| §4.3 never blocks | With the access policy at every decision, stop the writer; run 10⁵ operations. Assert operation latency unchanged within noise, `events_dropped_total{reason="ring_full"}` equal to the events beyond the ring, and, once the writer resumes, one gap record covering exactly those sequence numbers. | a blocking sink; a silent drop |
| §4.4 coalescing | Hammer a full quota with 10⁴ writes in a minute. Assert one hard-refusal record carrying the count, not 10⁴. | a quota stream that floods |
| §4.5 restart | Kill the process with events in the ring. Restart: assert a `restart` gap record and a new boot ID, and no sequence number reused within a boot. | a reused sequence after a crash |
| §4.6 retention | Fill the spool past `events.max_bytes`: assert the oldest file deleted, a `trimmed` gap record, `event_spool_high` raised first. Put `events.path` on a journal's filesystem: assert the start refused. | a spool that eats journal capacity |
| §4.8 access | As `viewer` and as `operator`, read the access and admin streams: refused. Change a share's audit policy: assert an `admin` record in the spool. Assert spool modes 0600/0700 and a start refused on a world-readable path. | audit data readable by any account |
| §4.3 admin stream | Kill the node between an audited change and its export; restart. Assert the record is in the spool once the writer resumes, and nothing after the cursor is missing. | an admin record lost to a crash |
| §5.4 never logged | Configure every secret-bearing record, plant known file names and known xattr contents, run the full suite at `Info`, and again at `Debug` for the second check. Search every line, span, health answer, metric and record: assert no secret anywhere, no content anywhere, no planted name at `Info` or above or outside the access stream. | a path in a warning; a token in a span |
| §5.2 per-request lines | Run 10⁵ operations of every kind with clients provoking every expected error. Assert the count of lines at `Info` and above does not grow with the operation count. | a log line per request |
| §5.2 debug expiry | Turn debug on for one component; advance the clock 15 minutes: assert it is off. | debug left on |
| §6.2 propagation **(cluster)** | Forward a write from P1 to S2 with sampling at 1: assert one trace with P1's span parenting S2's service span. Retry the write: assert the same request ID, a new span, and one application. | trace context in the dedup key |
| §6.3 cost | Count allocations per `Write`, `Read` and `GetAttr` with tracing off and with an endpoint at ratio 0: assert equal. | a span that allocates unsampled |

## 12. Open questions

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

## Appendix A — prior art

*Non-normative.*

- **Prometheus and OpenMetrics naming** — base units, `_total` on counters, unit
  suffixes, and the warning against unbounded label values: the source of §2.1–§2.4.
  OpenMetrics' state set and info types are §2.3's.
- **OpenTelemetry semantic conventions and the W3C Trace Context** — span naming,
  attribute names, and the `traceparent` encoding §6.2 means by "the standard
  trace-context encoding"; its exponential histograms are §2.6's upgrade.
- **Ceph's health checks** — named checks with a severity (`HEALTH_WARN`,
  `HEALTH_ERR`), the cluster status as the worst of them, and muting a check for a
  time without hiding it: the model for §3.1–§3.4 and silencing.
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
| §3.6, §3.7 | readiness false only while starting, draining or with the store lost; no detail without credentials | readiness answers 200 whenever the runtime exists, and reports share and store counts, running adapters and NFS grace state, unauthenticated (`internal/controlplane/api/handlers/health.go`, `pkg/controlplane/api/router.go`) |
| §4 | three spooled event streams | no access, quota or administrative event stream exists; no event sink type |
| §5.3 | trace ID on every request-scoped line | the logger defines `trace_id` and `span_id` keys but nothing sets them, and its context-taking functions ignore the context (`internal/logger/fields.go`, `internal/logger/logger.go`); the management API logs a request ID from its router middleware |
| §5.4 | no user-chosen name at `Info` or above | SMB handlers log paths and names at `Warn` on write, commit and create failures (`internal/adapter/smb/handlers/write.go`, `create_complete.go`); secrets are redacted in API answers only (`internal/controlplane/api/handlers/helpers.go`), not by the logger |
| §5.2 | debug turns itself off | the level is set from configuration or the environment and stays (`internal/logger/logger.go`) |
| §6 | spans per request and per service call | no tracing; the tracing library is only an indirect dependency (`go.mod`) |
| — | troubleshooting by condition | the troubleshooting guide's monitoring advice is to enable the metrics endpoint and grep the log for "slow" or "timeout" (`docs/guide/troubleshooting.md`) |
