---
rfc: 23
title: "RFC 23 — the control plane"
component: control-plane
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-9-gc]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-18-identity]]"
  - "[[rfc-19-authorization]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-25-observability]]"
  - "[[rfc-26-catalog-backups]]"
  - "[[rfc-27-namespace-migration]]"
  - "[[rfc-30-kubernetes]]"
aliases:
  - RFC 23
  - control plane
  - management API
tags:
  - rfc
---
# RFC 23 — the control plane

**Status:** draft. [§13](#13.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone adding a management-API resource or action, changing how a
node is composed or how it applies a record, or writing a client of the API —
the CLI, the Kubernetes operator, a script.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix B](#Appendix%20B%20%E2%80%94%20where%20the%20current%20code%20differs) says where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

The **control plane** is how an operator changes what a DittoFS installation
serves: which shares exist, who may reach them, where their content goes, which
protocols listen. It has two halves.

- The **management API** is the one door for every change. It authenticates the
  operator, checks the change, and writes it as records in the metadata store
  ([RFC 16](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
  It never reaches into a running component.
- The **node** — the one object each process builds — reads those records and
  makes its components match them: it adds a share's context to the engine,
  starts or drains an adapter, applies a live setting. It builds every
  component once at start and then follows the records.

Think of a restaurant: the API is the order desk, the metadata store is the
ticket rail, and each node is a cook who reads the rail. Where the
picture breaks: an order the desk would refuse — a setting bound to stored
content ([RFC 13 §5.1](rfc-13-configuration.md#5.1%20A%20bound%20setting%20refuses%20change)) — never reaches the rail at all.

[RFC 13](rfc-13-configuration.md) says what a setting is, how far it reaches and
what changing it does. [RFC 16](rfc-16-metadata-store.md) says what the records
are. This RFC says how they are written and how a node applies them: the
resources and their lifecycles, the API's shape and its rules for concurrent
operators, long work and retries, who may call it, what is audited, and how a
node composes and changes its components.

**The problem, with one example.** Two operators look after `builds`. At 10:00
Dana opens the share's export policy to add build02's subnet. At 10:01 Eli opens
the same policy to switch it to `sec=krb5`. Dana saves at 10:02, Eli at 10:03.
Eli's copy was read before Dana's save, so his write puts back the old client
list: build02 is refused, and nobody knows why, because both writes returned
success. Then Eli deletes the old `profiles-test` share. The call takes longer
than his client's 30 s timeout; the client retries; the retry finds the share
gone and reports an error, though the deletion is running. He cannot see how far
it has got, and when the node restarts an hour later he cannot tell whether it
will finish.

Under this RFC:

1. Every resource read returns an **entity tag**. Eli's write carries the tag
   he read, which Dana's save has moved on; the API
   refuses it with "changed since read" (`412`), and his client shows him
   Dana's version to merge into (§4.4).
2. Every create and every action carries an **idempotency key**. Eli's retry
   carries the same key and gets the first call's answer back, not a second
   deletion (§4.5).
3. A deletion is a **long-running operation**: the first call answers `202`
   with an operation ID. The operation's progress is read from the removal's own
   durable cursor ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)), so it survives the restart and
   any node can report it (§4.6).
4. Both operators' writes, refusals included, are in the **audit log** with
   who, when, from where and the generation before and after (§6).

**The words you need.**

- **node** — `node.Node`, built by `node.New`: it builds every component of the
  process, starts and stops them, and reconciles them with the records (§2).
- **resource** — one thing the API names: a share, a user, a remote store, an
  operation. Each maps onto records RFC 16 defines (§3.1).
- **generation** — the counter every record carries, raised by every change
  ([RFC 13 §2.3](rfc-13-configuration.md#2.3%20A%20record%20is%20versioned)); the entity tag of most resources (§4.4).
- **position** — the value of its kind's counter that a record's last change
  raised; how "applied" is computed, and a quota's entity tag (§2.4, §4.4).
- **node-status record** — the one record each node writes about itself: what
  it has reconciled, refused and holds restart-pending (§2.4).
- **operation** — a resource that stands for work longer than one request, with
  a state and progress, readable from any node until it ends (§4.6).
- **API account** — a `User` record that holds an API role. Its role grants
  nothing on files ([RFC 18 §8](rfc-18-identity.md#8.%20Management-API%20users)); file access comes only from share grants (§5).
- **role** — `admin`, `operator` or `viewer`: what an API account may do
  through the API (§5.2).
- **audit record** — the durable record of one management action or sign-in,
  written in the transaction of the change it records (§6).

**What this RFC promises.**

- Every change to what an installation serves goes through the management API
  and is written to the metadata store; no component is changed by any other
  path, and the CLI is only a client of the API.
- A concurrent edit is never lost: a write that did not read the current
  generation is refused.
- A retried request is never applied twice, and work longer than one request is
  an operation that any node reports and that survives a restart.
- A role grants nothing on files, and the bootstrap administrator is never
  uid 0.
- Every management action, refused or applied, leaves an audit record, and an
  applied one cannot exist without it.
- Every node applies a change within RFC 13's bounds and reports the generation
  it runs, so "applied everywhere" is something an operator can read.

**How the rest is organised.** §1 gives the purpose and non-goals. §2 is the
node: composition, start and stop, and how it applies records. §3 is the
resources and their lifecycles — shares above all. §4 is the API's shape:
resources, reads, writes, concurrency, idempotency, operations, errors. §5 is
authentication and authorization of API accounts; §6 the audit; §7 the CLI; §8
cluster-wide behaviour; §9 versioning. §10–§12 are invariants, metrics and the
test plan; §13 lists open questions. Appendix A is prior art, Appendix B where
the current code differs.

### Scenarios

Each runs on a single node, N1, unless marked **(cluster)**.

**The first start.**

1. The explicit initialisation step ([RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap)) creates the installation
   and the bootstrap account `admin`, with role `admin`, a password generated
   at random and printed once to the step's own output (§5.4).
2. `admin` holds no uid, gid, SID or Kerberos name and no share grant, so it can
   mount nothing ([RFC 18 §8](rfc-18-identity.md#8.%20Management-API%20users)).
3. N1 starts. The first sign-in as `admin` succeeds but returns a token that can
   only change the account's password (§5.4).
4. After the change, `admin` creates `profiles`, grants alice and bob
   `read-write`, and enables the SMB adapter.

Outcome: an installation is never reachable with a password nobody chose, and
its administrator reads no file it was not granted.

**Eli deletes `profiles-test`, and the client times out.**

1. `DELETE /api/v1/shares/sh_7Q…` with an idempotency key. The API runs
   `DeleteShare`'s first transaction — the mark — and answers `202` with
   operation `op_3c…` (§4.6).
2. The client's 30 s timeout fires before the answer arrives; it retries with
   the same key. The API finds the key's record and returns the stored `202` and
   the same operation (§4.5).
3. `GET /api/v1/operations/op_3c…` shows `running`, phase "files", 41,200 of
   about 90,000 files dropped.
4. N1 restarts. The removal resumes from its own record; the operation reads
   `running` again, its counts continuing from where they stopped.

Outcome: one deletion, visible throughout, finished after the restart.

**Two operators edit the export policy of `builds`.**

1. Dana and Eli each `GET` the policy; both see the same `ETag`.
2. Dana's `PUT` with `If-Match` and that tag commits, and the policy has a new
   tag.
3. Eli's `PUT` with `If-Match` and the old tag is refused `412`, code
   `generation_mismatch`, naming the current tag. The CLI re-reads, shows the
   difference, and asks Eli to apply his change to Dana's version.

Outcome: no write overwrites one its author never saw.

**(cluster) An operator changes the `atime` policy of `profiles` on N1.**

1. The API on N1 validates the value with the share component's own validator
   and commits the setting, raising the settings generation, and the audit record,
   in one transaction (§4.3).
2. Every node reads the settings generation each second, re-reads the record,
   and applies it, live, within 5 s ([RFC 13 §5](rfc-13-configuration.md#5.%20Binding%20classes)).
3. Each node reports the position it has reconciled through. `GET` on the
   setting shows `applied: 3 of 3 nodes` — N1, N2 and N3; had N3 been frozen,
   it would show N3 behind and the `config_generation_behind` condition naming
   it.

Outcome: one write, and an answer to "is it applied everywhere?" that is read,
not guessed.

## In short

- `node.New` builds every component of a node, and the node then follows the
  records. Start brings up the store, the content side, the filesystem service,
  the API, then the adapters; stop is the reverse. Every node runs both roles.
- The API writes records; it never calls a component. Every node
  applies records by their binding class and reports the generation it runs.
- A resource is addressed by ID. Reads are consistent; lists page by an opaque
  cursor. Every update and delete carries `If-Match` with the `ETag` read;
  every create and action carries an idempotency key, except the credential
  calls (§4.5).
- Work longer than one request is an operation: `202`, an operation ID, progress
  read from the owning component's durable cursor, readable from any node, and
  resumed after a crash by the component that owns the work.
- Errors are the components' own error values, carried as problem details with
  the field and the rule; one table maps each to an HTTP status.
- API accounts are `User` records with a role: `admin`, `operator`, `viewer`.
  Roles grant nothing on files. Tokens are short-lived and checked against the
  account on every call, so disabling an account takes effect within a second.
- Every action is audited in the transaction that applies it. The CLI is a
  client of the API and nothing else.

---

## 1. Purpose

The storage and protocol RFCs each name records an operator changes — a share's
state, a grant, a remote store's credential, a snapshot policy — and calls an
operator makes: a rebuild of GC's index ([RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)), a statement that a
backup location is lost ([RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record)), a re-home ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)).
Each defers the door to this RFC. Without one owner, each resource grows its own
answer to the same questions — what a retry does, what two concurrent edits do,
who may call it, what is recorded — and the answers differ. This RFC answers
them once:

- the node: how it is composed, started, stopped and kept matching its
  records (§2);
- the resources and their lifecycles (§3);
- the API's shape and its rules for concurrency, retries and long work (§4);
- authentication and authorization of API accounts (§5) and the audit (§6);
- the CLI's place (§7), cluster-wide behaviour (§8) and compatibility (§9).

### 1.1 Non-goals

This RFC **MUST NOT** be read as specifying:

- what a setting is, its scope, its binding class, validation or secrets: those
  are [RFC 13](rfc-13-configuration.md)'s, which this RFC applies and does not restate;
- the records and their keys: [RFC 16](rfc-16-metadata-store.md)'s; this RFC
  names which records a resource writes, through RFC 16's `ControlPlane` view;
- what a component does with a record — a share's removal steps, a snapshot's
  cut, a move — which stays in the component's RFC;
- the identity of file principals, mapping and squashing:
  [RFC 18](rfc-18-identity.md)'s. This RFC owns only how an API account signs
  in to the API — the password check itself it asks of identity directly
  ([RFC 18 §8.1](rfc-18-identity.md#8.1%20The%20API%27s%20sign-in%20interface)) — and what its role permits there;
- what a caller may do to a file: [RFC 19](rfc-19-authorization.md)'s;
- metric and label conventions, how health is derived, and the delivery of
  events: [RFC 25](rfc-25-observability.md)'s. This RFC owns the management
  API's surface, including that health and readiness endpoints exist and who may
  call them;
- a node that runs only the `protocol` or only the `storage` role: every node
  runs both, as [RFC 15](rfc-15-topology.md)'s `ponytail:` marker records.

## 2. The node

### 2.1 The node builds its components

`node.New(cfg)` is the one composition root: the server's start command calls
it, and it alone names concrete component types. It builds every component from
that component's own `Deps`, in the order below, and returns the **node**, a
`*node.Node` that holds them all. `New` opens only the metadata store and
starts nothing: every view it hands out is bound to that store, so `New` calls
`metadata.Open` ([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)) before it builds anything that takes one. No
constructor takes settings: each component's first settings arrive through its
`Apply`, which the node calls once it has read the records (§2.2 step 4) and
before it calls that component's `Start`. From then on the node is the only
code that starts, changes or stops a component:

- it adds, replaces and removes a share's context in the engine
  ([RFC 8 §2.3](rfc-8-engine.md#2.3%20One%20engine%20per%20node%3B%20a%20share%20is%20a%20context)) — adding, removing and quiescing a share never restarts
  the engine;
- it moves each adapter between configured, serving, draining and stopped, and
  reconfigures its listeners ([RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states));
- it runs the sidecars, calling each one's `Sync` with the adapters it serves
  that are serving ([RFC 20 §3.5](rfc-20-adapters.md#3.5%20Sidecars));
- it hands a live setting to the component that reads it;
- it runs the operations no other component owns (§4.6).

A component **MUST NOT** build another component or close anything it was
handed, and the API **MUST NOT** call a component to change it: both go through
records the node applies. A handler that called the engine to add a share
"because it is quicker" would make the share's existence depend on which node
took the request.

> decision: the API writes and the node applies, even on a single node where
> a direct call would be shorter. A direct call is a second path: it runs on one
> node, it can succeed while the record write fails or the reverse, and a
> restart replays only the record. Revisit only if a change must take effect
> faster than the one-second generation read, and then shorten the read, not the
> path.

> ponytail: the node both builds and drives its components. Split the builder
> out when it gains dependencies a multi-node test cannot fake cheaply.

**The node is never on the request path.** `New` hands each adapter a
`vfs.Service` once, and an adapter's call goes to that service, never to the
node. The management API calls the node for nothing: it writes the
control-plane store, and the node applies the change through reconcile (§2.3).
So every change takes effect by one path — API, store, reconcile, the
component's call — on every node alike. The API's one direct call is to
identity, to check a password and an account
([RFC 18 §8.1](rfc-18-identity.md#8.1%20The%20API%27s%20sign-in%20interface)), which changes nothing. The node holds no routing or cluster
logic: in a cluster, the one thing `New` does differently is build `vfs.Serve`
and the gateway.

![The node and what it builds. On the left, the management API writes records to the control-plane store, and the node reads its generations and drives every component: New builds them, Start starts them in build order, Stop stops them in reverse but hands the node's shards over before Serve stops, and reconcile calls addShare, removeShare and apply; the node is never on the request path. On the right, the components in build order from the bottom: 1 the metadata store and 2 the remote stores; 3 the syncer, beside the journals the engine opens itself; 4 the engine and 5 GC, its sibling, which is never passed into it; 6 open state and 7 identity; 8 the vfs server; 9, in a cluster only, vfs.Serve and the gateway; 10 the adapters. The arrows are calls: the management API calls identity for sign-in only; the adapters call the vfs service alone, sign-in included; the gateway calls this node's Serve locally, or another node's Serve over the transport, using the router, so a local call is fenced too; Serve fences and deduplicates, then calls the local server; the server calls identity, open state, the metadata views and the engine; the engine calls the journal, the syncer and the metadata views; GC calls the metadata store's Blocks view, the syncer and the remote stores; the syncer calls the remote stores](img/rfc23-node.svg)

**What `New` builds, in order.** Each row is built from its own `Deps`; the
Calls column is the call graph once it is built. The node owns every row and
closes it at the stop step named (§2.2); the remote stores are ref-counted per
store configuration, and the last share's context to close one releases it.

| # | Component | `Deps` | Calls | Start / stop step (§2.2) |
| --- | --- | --- | --- | --- |
| 1 | metadata store ([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)) | the KV, the host's bootstrap | the KV | opened by `New` / 9 |
| 2 | remote stores ([RFC 4 §4.10](rfc-4-remote-tier.md#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)), one client per store configuration | the store configuration, its `Secret` | the remote service | 5 / 8 |
| 3 | syncer ([RFC 3](rfc-3-syncer.md)) | the remote stores | the remote stores | 5 / 8 |
| 4 | engine ([RFC 8 §2.6](rfc-8-engine.md#2.6%20Dependencies%20and%20wiring)) | `Devices` and `Open`, the journal opener; the carver; the syncer; the `Existence`, `Content` and `Lifecycle` views | the journals, the syncer, the block-metadata views | 5 / 8 |
| 5 | GC ([RFC 9 §2.5](rfc-9-gc.md#2.5%20Composition%20and%20lifecycle)), a sibling of the engine, never passed into it | its scheduler, the syncer; at each namespace's `Add`, the `View` of its `Blocks`, `Remote` and `Keys` | `Blocks`, `Remote`, the syncer, `Keys` | 5 / drains at 5, closes at 7 |
| 6 | open state ([RFC 14](rfc-14-open-state.md)) | `Store`, the metadata store; at `Start`, its `Sink`, which the server implements | the metadata store, the `Sink` | 5 / 7 |
| 7 | identity ([RFC 18 §2.6](rfc-18-identity.md#2.6%20Components%2C%20wiring%20and%20lifecycle)), built once and shared by row 8 and the management API | RFC 18 §2.6's `Deps` | the metadata store, the directory | 5 / 7 |
| 8 | `*vfs.Server`, by `vfs.New` ([RFC 17](rfc-17-vfs.md)) | `Meta` (the metadata store), `Open` (open state), `Engine` (the engine); from identity, `Identity` (its `Resolver`), `Auth` (its `Authenticator`) and `Names` (its `Identities`) | identity, open state, the metadata views, the engine | 6 / 6 |
| 9 | **(cluster)** `vfs.Serve`, then the gateway by `vfs.NewGateway` ([RFC 17](rfc-17-vfs.md)) | `Serve`: the server, the `Transport`, the `Router`; the gateway: `Local` (the checked `Service` that `Serve` returns), `Router`, `Transport` | `Serve`: the fence, the dedup, then the server; the gateway: `Service`, `Router`, `Transport` | 6 / 4 |
| 10 | adapters ([RFC 20 §3.4](rfc-20-adapters.md#3.4%20Interface)) | `Service`, the `vfs.Service` — the gateway in a cluster, the server on one node — and nothing of identity | `vfs.Service`, sign-in and principal spellings included | 9 / 2 |

**What `New` looks like.** The sketch is illustrative and elides errors and
the values beside the rows. Where a component's RFC declares a constructor or a
`Deps` field, the sketch uses that name; where none is declared yet (the remote
stores, the syncer and the API), it names one.

```go
func New(cfg Config) (*Node, error) {
	n := &Node{}
	n.meta   = metadata.Open(context.Background(), cfg.KV, metadata.Options{...}) // RFC 16 §3.2; the one thing New opens; everyone takes a view of it; closed last
	n.remote = remote.NewPool(remote.Deps{...})                         // one client per store configuration, ref-counted
	n.syncer = syncer.New(syncer.Deps{Remote: n.remote, ...})
	n.engine = engine.New(engine.Deps{
		Devices: cfg.Bootstrap.Devices, Open: journal.Open, Syncer: n.syncer, // the engine opens its journals at Start
		Existence: n.meta.Existence(), Content: n.meta.Content(), Lifecycle: n.meta.Lifecycle(), ...})
	n.gc     = gc.New(gc.Deps{Sched: sched.New(cfg.Clock), Syncer: n.syncer, ...}) // a sibling of the engine, never passed into it
	n.open   = openstate.New(openstate.Deps{Store: n.meta, ...})         // its Sink, the server, is handed to its Start (§2.2 step 5)
	n.ident  = identity.New(identity.Deps{Principals: n.meta.Principals(), ...}) // no settings: the first Apply brings them
	n.server = vfs.New(vfs.Deps{Meta: n.meta, Open: n.open, Engine: n.engine,
		Identity: n.ident, Auth: n.ident, Names: n.ident, ...})                // the one identity, behind three narrow fields

	var svc vfs.Service = n.server                                      // one node: the adapters call the server
	if cfg.Transport != nil {                                           // (cluster); n.router is RFC 15's
		checked := vfs.Serve(n.server, cfg.Transport, n.router, ...)    // the fence and the dedup around the server
		svc = vfs.NewGateway(vfs.GatewayDeps{Local: checked, Router: n.router, Transport: cfg.Transport})
	}
	n.adapters = core.Deps{Service: svc, ...}                           // handed to each adapter's factory as reconcile enables it; no identity
	n.api      = api.New(api.Deps{SignIn: n.ident, ...})                // the same identity, called directly, never through vfs (RFC 18 §8.1)
	return n, nil                                                       // only the metadata store opened; nothing started
}
```

`Start` registers the node (§2.2 step 3), applies the records' settings to
every component (step 4), and then starts the rest in build order; `Stop`
hands over the node's shards before `vfs.Serve` stops, and otherwise stops everything in reverse.

**A call routed locally is fenced too.** In a cluster the gateway's `Local` is
the checked service `vfs.Serve` returns, never the bare `*vfs.Server`, so a
call the gateway keeps on this node passes the node epoch, shard epoch, hop
count and dedup checks a forwarded call passes. Without them a stale "this
node is the primary" could act unfenced, and a retry first sent locally could
apply twice after a takeover. On one node there is no gateway and no fence.

**The node builds no journal.** The engine opens each device's journal itself,
through the opener the node passes as `Open`, once it has read the version floor
and the journal identity the store records ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)); it closes them at its
stop. A journal built before that read could open with the wrong floor.

Beside the rows: the clock, the fault seam ([RFC 8 §2.1](rfc-8-engine.md#2.1%20Content%20composition)), the
metrics registry, the logger and the pools ([RFC 24 §2.3](rfc-24-resources.md#2.3%20The%20pools)) are values built
first and live until exit; the event sink ([RFC 25 §4.3](rfc-25-observability.md#4.3%20From%20the%20emit%20point%20to%20the%20spool)) is built with
identity and closed after the server; the operation runner (§4.6) and the API
address (§2.5, §4) are built after row 9 and stop after the adapters; the
sidecars ([RFC 20 §3.5](rfc-20-adapters.md#3.5%20Sidecars)) come with the adapters; and, **(cluster)**, shard placement
([RFC 11](rfc-11-ownership.md)) comes before the engine, and the client-address agent and the pNFS data server
([RFC 15](rfc-15-topology.md)) with row 9. Two things are not built by `New`, because records decide
them: share contexts, which the node attaches through the engine, and each
adapter the records enable, which the node builds through its factory
([RFC 20 §3.4](rfc-20-adapters.md#3.4%20Interface)).

**The carver is not a component.** It holds no state ([RFC 2](rfc-2-carver.md)). The engine's
offload pipeline calls its `Cut`, at the pass's Carve stage
([RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline)); it reaches the engine as a value in its `Deps`, and the
node starts and stops nothing for it.

**The node's surface.**

```go
package node // internal/node

// New builds every component of §2.1's table, in its order, each from its own
// Deps. It opens only the metadata store, and starts nothing.
func New(cfg Config) (*Node, error)
func (n *Node) Start(ctx context.Context) error // §2.2's start, then reconcile until Stop
func (n *Node) Stop(ctx context.Context) error  // §2.2's stop: the components in reverse build order

// Config is what the host supplies and the node cannot build.
type Config struct {
	Bootstrap config.Bootstrap // RFC 13 §2.2
	KV        kv.KV            // what the metadata store opens on; an in-memory one in a multi-node test
	Transport vfs.Transport    // (cluster): nil on one node; a loopback one in a multi-node test
	Clock     clock.Clock
	Fault     Fault            // RFC 8 §2.1
	Metrics   *metrics.Registry
	Logger    *slog.Logger
}

// Records is the node's view of the control-plane store; not RFC 16's
// ControlPlane, which is the API's write view.
type Records interface {
	RegisterNode(ctx context.Context, n NodeRecord) error                      // §2.2 step 3: roles and version ranges, refused in the same transaction if an active version lies outside them (RFC 13 §5.5)
	List(ctx context.Context, kind Kind, after string) iter.Seq2[Record, error] // paged (RFC 16 §3.1)
	Counters(ctx context.Context) (Counters, error)                            // settings, share list, principals
	PutNodeStatus(ctx context.Context, s NodeStatus) error                     // §2.4
}

// Reconcile's three calls (§2.3). Unexported, so nothing but reconcile makes them.
func (n *Node) addShare(ctx context.Context, s metadata.Share) error      // a share context in the engine, its namespace in GC, the share open in *vfs.Server
func (n *Node) removeShare(ctx context.Context, id metadata.ShareID) error // the same three, in reverse
func (n *Node) apply(ctx context.Context, c Counters) error                // Apply on identity, *vfs.Server, the engine and GC, each with the values it reads
```

The node exports no status call. Readiness and conditions come from
[RFC 25](rfc-25-observability.md#3.%20Health)'s health registry, which `New`
hands the API, so the API never calls the node and the node, which holds the
API, forms no cycle with it.

> decision: the node exports `New`, `Start` and `Stop` only, and is not
> restartable: a second `Start` is an error. Reconcile's calls are unexported,
> so no caller, the API included, can change a component by a second path.
> Overturned if something other than the server's start command and the test
> harness must drive a node, and then by a second root, not by exporting the
> calls.

### 2.2 Start and stop

**Start.** `Node.Start` runs these steps, each completing before the next
begins. Steps 5, 6, 7 and 9 start the components in build order (§2.1):

1. **Refuse** what [RFC 15 §2.2](rfc-15-topology.md#2.2%20Startup%20refuses%20what%20cannot%20work) lists, and a missing or foreign
   installation ([RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap)). `New` has already refused both: the first
   before it opens the metadata store, the second as `metadata.Open` reads the
   format record ([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)). A start creates nothing.
2. **Open the listeners.** From here the node's health endpoints answer
   (§2.5): live, not ready. The node's API address and the metrics listener
   open here. Until step 7, every `/api/v1` route but `/api/v1/health` answers
   `503` `starting`.
3. **Register the node**: its version ranges and roles in its node record
   ([RFC 13 §5.5](rfc-13-configuration.md#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)), refused in the same transaction if an active version lies
   outside them. A refusal stops the node here, before the engine's start
   transaction runs or any adapter serves.
4. **Read the records**: the settings, the share list and the principals
   generation, and apply the provisioning file if one is named
   ([RFC 13 §2.4](rfc-13-configuration.md#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)). Then `apply` (§2.3) hands identity,
   `*vfs.Server`, the engine and GC their first settings through `Apply`, so
   every component holds the records' values before any of them starts.
5. **The content side and the state the server reads**, in build order:
   **(cluster)** shard placement ([RFC 11](rfc-11-ownership.md)); the engine, in
   [RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)'s order, handed the whole set of shares this node serves in
   one call; GC, with each served share's namespace; open state for the shards
   this node acquires, handed the built server as its `Sink`
   ([RFC 14 §11.2](rfc-14-open-state.md#11.2%20Lifecycle)); identity and the event sink. Only a share
   created while the node serves is attached alone.
6. **`*vfs.Server`** (`Start`, [RFC 17](rfc-17-vfs.md)); then, **(cluster)**, `vfs.Serve`, the
   gateway, the client-address agent and the pNFS data server.
7. The **operation runner** (§4.6), which resumes the operations this node
   holds; then the **management API**, which starts accepting calls.
8. **The first full reconcile** (§2.3) completes, and, **(cluster)**, the node
   holds a live node lease and its node epoch
   ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
9. The **adapters**, each from configured to serving as its records say, then
   the **sidecars**, synced with the adapters now serving. An adapter listens
   only from here, so no client reaches a node that has not reconciled or has
   no node epoch to stamp, and no call is answered retry-later while the node
   starts.
10. **Reconcile** (§2.3) until `Stop`.

Readiness turns true once every adapter the records enable is serving.

**Stop.** `Node.Stop` stops the components in reverse build order, each step
completing before the next, except that the node's shards are handed over
before `vfs.Serve` stops (step 4), while other nodes can still reach it:

1. Readiness turns false, so whatever steers clients stops sending them
   ([RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states)), and reconcile stops.
2. The sidecars are synced with the empty set, so nothing is advertised that is
   about to stop; then every adapter is `Stop`ped, which drains it
   ([RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states)), all of them concurrently, under one drain deadline.
3. The management API stops accepting calls, and calls in flight run to their
   answer within the drain deadline; then the operation runner stops, leaving
   each operation it drives at its last durable step and releasing its claim
   (§4.6).
4. **(cluster)** The shards this node is primary of are handed over
   ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) while `vfs.Serve` still receives, so another node's
   call for one of them meets a refusal naming the new primary instead of
   waiting for a failover. Then the pNFS data server, the client-address agent
   and the gateway stop, then `vfs.Serve`.
5. GC drains ([RFC 9 §2.5](rfc-9-gc.md#2.5%20Composition%20and%20lifecycle)).
6. `*vfs.Server`'s `Drain`, then its `Close` ([RFC 17](rfc-17-vfs.md)).
7. The event sink, identity and open state close, and then GC.
8. The engine stops in RFC 8 §2.5's reverse order, which joins and closes every
   share context, its journals and its flows; then the syncer and the remote
   stores close.
9. The metadata store closes last.

The API outlives the adapters on purpose: an operator watching a drain reads its
progress through the API. A failure at any start step stops the start, closes
what the earlier steps opened in reverse, the metadata store `New` opened last, and leaves `/health/ready`
answering `not ready` and `/api/v1/health` naming the step and its error until
the process exits.

**A stop step that misses its bound** leaves open every component that a later
step would close under it, and the stop goes on to the steps that do not depend
on it ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing), step 5). The process then exits non-zero, naming
the step. The next start recovers as after a crash, so a stop never closes a
store under a live loop in order to exit cleanly.

### 2.3 The node reconciles; it does not take orders

The node holds two pictures: the **desired** one, read from records, and the
**running** one, the components it built. It acts only on the difference:

| Record changed | The node, on each node it concerns |
| --- | --- |
| a share created | `addShare`: a share context in the engine, the share's namespace in GC, the share open in `*vfs.Server`; admission reads the share list through `ShareList.Version` ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)) |
| a share removed | `removeShare`, at the step its removal names ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)): the same three, in reverse |
| a share's state changed | moves the share's context to quiescing and back ([RFC 8 §2.3](rfc-8-engine.md#2.3%20One%20engine%20per%20node%3B%20a%20share%20is%20a%20context)); admission reads the state itself on every call ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)), and a removal's steps run in their owner ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)) |
| a share's live setting | hands the new value to the context, within RFC 13's live bound |
| a remote store's live or next-write setting — endpoint, credential reference, storage class, block target, the transform chain but its encrypt stage, as [RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings) classes them | is applied once per store, to the component that reads it: endpoint, credential reference and storage class to the one store client every context on that store shares ([RFC 4 §4.10](rfc-4-remote-tier.md#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)), after [RFC 13 §5.2](rfc-13-configuration.md#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) has proven a new endpoint or credential; the block target to the block assembler ([RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler)) and the transform chain to the engine's codec ([RFC 5 §2.2](rfc-5-transforms.md#2.2%20Where%20it%20runs)), GC's relocation included, for the store's contexts. No share context is replaced |
| a `Secret`'s new version | the component holding the reference resolves it again within the live bound, and the node reports the version it holds (§2.4, [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)) |
| a change [RFC 8 §2.1](rfc-8-engine.md#2.1%20Content%20composition) counts as a change of a share's backends | builds the new context beside the old, switches admission to it, then joins and closes the old one. A new context that fails to build leaves the old one serving and the record refused |
| an adapter enabled or disabled | starts it, or drains and stops it |
| an adapter's live setting | applies it to connections set up afterwards ([RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states)) |
| an adapter's restart-class setting | builds a new adapter through its factory with the new settings. `New` opens nothing, so a setting it refuses leaves the old adapter serving and the record refused. Only then the node drains and stops the old adapter and starts the new one; the others keep serving. If the new one fails to start, the node builds and starts one with the last settings that served, and reports the record refused (§2.4) |
| a sidecar enabled or disabled | `Sync` with the serving adapters, or with none ([RFC 20 §3.5](rfc-20-adapters.md#3.5%20Sidecars)) |
| a node-scoped or installation-scoped restart-class setting | nothing until the process next starts; the node reports the record as restart-pending (§2.4) |
| a principal, grant or export policy | `apply`, handing identity and `*vfs.Server` the new generation; their generations and versions are in the identity and admission cache keys ([RFC 18 §7.2](rfc-18-identity.md#7.2%20Local%20changes%20take%20effect%20within%20a%20second)) |
| a shard record | nothing: the gateway's `Router` reads shard records itself, and [RFC 11](rfc-11-ownership.md)'s epochs keep its routing correct |

Every row is one of reconcile's three calls (§2.1): a share created or removed
is `addShare` or `removeShare`, a change of its backends is both, and every
other row is `apply`, which hands each component — identity, `*vfs.Server`, the
engine, GC, the adapters and the sidecars — the values it reads.

The node learns of changes the way RFC 13 and RFC 18 already require — by
reading the three counters (the settings generation, the share list version and
the principals generation) once a second — and never from the API request that
made them. A record that does not
validate when the component is built stops only what it configures
([RFC 13 §6](rfc-13-configuration.md#6.%20Validation)), and the node reports it refused.

A reconcile step is idempotent: run twice on the same records it does nothing
the second time. So the node **MUST** also run a full reconcile at start and
after any read of the store that failed, not only on a moved generation: a
change missed during a store outage is then found at the next read that works.
A full reconcile lists records by kind in pages
([RFC 16 §3.1](rfc-16-metadata-store.md#3.1%20Lookups%20by%20ID%2C%20listings%2C%20and%20who%20may%20use%20them)); what bounds its rate is open (§13).

### 2.4 What a node reports

Each node writes, in its own node-status record, at most once a second and only
when a value changed:

- the **settings generation**, **share list version** and **principals
  generation** it has reconciled through: the highest value of each counter
  such that it has acted on every change up to it — applied it, refused it, or
  marked it restart-pending;
- the records it refused, each with the generation and the error;
- the records whose restart-class field has changed since it started — its
  restart-pending set;
- its health conditions, as [RFC 25](rfc-25-observability.md) derives them;
- the version of each secret it holds, so that an old version is deleted only
  once every node that resolves the secret holds the new one
  ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)).

A record's own generation (§4.4) counts its changes; it is not comparable with
these counters. So every control-plane record **MUST** also carry its
**position**: the value of its kind's counter that its last change raised,
written in that change's transaction. A record at position *S* is **applied**
on a node once that node has reconciled its kind through at least *S* and the
record is in neither its refused set nor its restart-pending set. The API joins
these reports into each resource's status (§4.1), so "applied on 2 of 3 nodes;
N3 behind by 7 s" is a read, not an inference.

> ponytail: a node reports three reconciled positions and the exceptions, not
> one generation per record, so the report stays one small record at 10⁴
> shares. The cost is that "applied" is inferred from a high-water mark, which
> holds only because the node acts on changes in counter order and lists
> every change it did not apply. Report per-record positions if a component
> ever applies records out of order.

### 2.5 Health and readiness endpoints

Every node serves on its API address these three endpoints:

| Endpoint | Answers | Authentication |
| --- | --- | --- |
| `GET /health/live` | `200`, with the one word `live`, while [RFC 25 §3.5](rfc-25-observability.md#3.5%20Liveness)'s watchdog holds; `503` otherwise | none |
| `GET /health/ready` | `200` when the node is ready, `503` when not, with one word of body: `ready`, `starting`, `draining`, `not_ready` | none |
| `GET /api/v1/health` | the node's health conditions and those of the installation, each with its cause, as [RFC 25 §3.7](rfc-25-observability.md#3.7%20What%20the%20health%20answers%20report) sets out | `viewer` |

The unauthenticated endpoints **MUST NOT** carry anything but that one word: no
share name, client count, grace state or version. A probe needs to know whether
to send traffic, and an anonymous caller is owed nothing more. What "live",
"ready" and each condition mean is [RFC 25 §3](rfc-25-observability.md#3.%20Health)'s; that a draining
adapter is not ready is [RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states)'s. The endpoints answer from step 2
of start (§2.2), so a node stuck recovering its journal says so instead of
refusing connections.

**The metrics listener** ([RFC 25 §2.8](rfc-25-observability.md#2.8%20Export)) has an address of its own, separate
from the API's, a bootstrap fact of the host
([RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap)). It is served without authentication, over the API's TLS on
every address but loopback, and opens at start step 2.

> decision: the metrics listener asks for no token, because a scraper must
> reach every node and holds no API account. What it
> exposes to anyone who reaches the address is RFC 25 §2.4's labels: share and
> remote store names and counts, never a file name, user content or a secret
> ([RFC 25 §2.8](rfc-25-observability.md#2.8%20Export)). Overturned if those names are themselves sensitive in
> a deployment, and then by binding the address to a management network or by
> requiring a token.

## 3. Resources and their lifecycles

### 3.1 The resources

Each resource maps onto records RFC 16 defines, and is written only through its
`ControlPlane` view ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)) or, for work a component owns, through
that component's surface:

| Resource | Records | Lifecycle | Written through |
| --- | --- | --- | --- |
| installation | `Installation`, the store format record | created by the initialisation step only; active versions raised by `:raise-format` | [RFC 13 §5.5](rfc-13-configuration.md#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap) |
| node | `Node`, node status | registered at each start; drained, decommissioned by the operator | [RFC 30 §5.2](rfc-30-kubernetes.md#5.2%20Scale-down%20is%20a%20drain), [RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries) |
| remote store | store configuration settings, its credential `Secret` | created; edited under RFC 13's classes; deleted only when no namespace names it | RFC 13 |
| namespace | namespace settings and key records | created with its bound fields; deleted when no share names it and drained | [RFC 9 §7.6](rfc-9-gc.md#7.6%20A%20namespace%20no%20share%20names%20is%20drained) |
| share | `Share` (info, state, namespaces), `ExportPolicy`, share settings | §3.2 | `ControlPlane` |
| grant | `ShareGrant` | set, `none` included; removed by its own call | `SetGrant`, `DeleteGrant` |
| quota | `Quota` | set; one that limits nothing deletes | `SetQuota` |
| user, group, membership | `User`, `Group`, `Membership` | §3.5 | `ControlPlane` |
| netgroup | `Netgroup` | put, delete | `ControlPlane` |
| setting | `Setting` at a scope | put; a default is the absence of a record | RFC 13 |
| secret | `Secret` | write-only: set, rotate, delete, each authorized as its owning resource's write (§5.2); never read | [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets) |
| adapter | adapter settings, installation scope with node override | §3.4 | RFC 13, [RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states) |
| snapshot, snapshot policy | `Snapshot`, policy settings | [RFC 12](rfc-12-snapshots.md#3.1%20Interfaces) | `Snapshots` |
| backup location, backup | its record, backup records | [RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record), [RFC 26](rfc-26-catalog-backups.md#4.1%20Interfaces) | `Backups` |
| move, re-home | their records | [RFC 27](rfc-27-namespace-migration.md#3.1%20Interfaces) | `Migration`, `Rehome` |
| operation | operation record | §4.6 | the API |
| API token, session | token and session records | §5.3 | the API |
| audit record | audit record | §6 | the API, in the change's transaction |

Operation, token, session and audit records, and the node-status record of
§2.4, are records this RFC adds; [RFC 16](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed) defines their keys.

### 3.2 A share

A share's states are RFC 16's ([§2.3.1](rfc-16-metadata-store.md#2.3.1%20Share%20names%2C%20paths%20and%20state)); this section says which API call
moves it between them and what the API returns.

![A share's states: create leads to enabled; enabled and quiesced move to each other by quiesce and resume; either moves to being removed by delete; being removed ends with no record, or with retired when snapshots are detached, and retired ends with no record when its last detached snapshot is deleted; rename, re-path and re-home loop on enabled and quiesced; under each state, what admission answers](img/rfc23-share-states.svg)

| Call | From | To | Answer |
| --- | --- | --- | --- |
| `POST /shares` | — | `enabled` | `201`; serving once its context is (below) |
| `POST /shares/{id}:quiesce` | `enabled` | `quiesced` | `200`; admission answers `ErrDelay` |
| `POST /shares/{id}:resume` | `quiesced` | `enabled` | `200` |
| `PATCH /shares/{id}` name or path | `enabled`, `quiesced` | same | `200`; one transaction raising `ShareList` |
| `POST /shares/{id}:rehome` | `enabled`, `quiesced` | same | `202`, an operation ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| `DELETE /shares/{id}?snapshots=…` | `enabled`, `quiesced` | `being removed`, then gone or `retired` | `202`, an operation ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)) |
| `POST /shares` with a `from` snapshot or backup | — | a new share | `202`, an operation (clone [RFC 12](rfc-12-snapshots.md#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace), restore [RFC 26](rfc-26-catalog-backups.md#2.3%20Restore)) |

**Create is one transaction.** `POST /shares` names the share's name, path,
namespace — an existing one, or the specification of a new one, created in the
same transaction — and its settings. The API validates the name and path under
RFC 16's rules, the settings with each component's validator, and refuses a
share whose namespace names no remote store ([RFC 8 §2.1](rfc-8-engine.md#2.1%20Content%20composition)). The transaction
writes the share's records, its root directory, its state `enabled`, its export
policy as the request states it — the flavours are required, with no default —
and raises `ShareList`; it writes **no grant**.
A new share is reachable by nobody until an operator grants it.

> decision: a new share admits nobody. An open default is one operator's
> forgotten step away from a share every client on the network can write.
> Revisit if a provisioning workflow shows the extra grant call to be the common
> error, and then make the default a named template, not an open one.

A share is **serving** on a node once that node has added its context
and the context reports serving ([RFC 8 §2.3](rfc-8-engine.md#2.3%20One%20engine%20per%20node%3B%20a%20share%20is%20a%20context)). Create returns at commit,
before that; the share's status shows each node's state, and `?wait=serving`
on a `GET` returns when every node that serves it reports serving or a node
refuses it.

**Quiesce and resume name their expected state.** Each is one
`SetShareState(from, to)` ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)), and needs no `If-Match`: the
expected state is its precondition. A quiesce of a share that is already
`quiesced` answers `200` and changes nothing; one of a share `being removed`
answers `409` with `ErrStale`.

**Delete is one-way and asynchronous.** The API runs `DeleteShare`, which
returns once the mark has committed — the name and path free, every call into the
share `ErrStale` — and answers `202` with an operation whose progress is the
removal's own state record (§4.6). The snapshot disposition is a required query
parameter when the share has snapshots; RFC 16 §2.3.2's refusals — a cut
running, a GC pause, locked snapshots under `delete` — come back as `409` with
the component's error.

**Rename and re-path are edits, not moves.** Both change only index rows and
raise `ShareList`; handles, opens and tree connects keep working by `ShareID`
([RFC 16 §2.3.1](rfc-16-metadata-store.md#2.3.1%20Share%20names%2C%20paths%20and%20state)). Neither changes a binding: a share's namespace changes only
by `:rehome` ([RFC 13 §5.1](rfc-13-configuration.md#5.1%20A%20bound%20setting%20refuses%20change)).

**Every write that names a share reads its state.** A `PATCH`, grant, export
policy, quota or share-scoped setting of a share `being removed` or `retired`
is refused `409` `ErrStale`, as RFC 16 §2.3.2 step 1 requires; the API adds no
check of its own beside that one.

### 3.3 Stores and namespaces

A **remote store** is a store configuration ([RFC 13 §3](rfc-13-configuration.md#3.%20Scopes)): endpoint,
bucket, credential reference, transform chain and the rest. A **namespace** is
created on one, with its bound fields — prefix, chunking settings, whether it
encrypts — fixed at creation. The API applies RFC 13's classes and nothing else:

- a bound field is refused `409` `bound_setting` while content exists, naming the
  field ([RFC 13 §5.1](rfc-13-configuration.md#5.1%20A%20bound%20setting%20refuses%20change));
- a new endpoint or credential is proven before it is written
  ([RFC 13 §5.2](rfc-13-configuration.md#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change));
- **rotating a credential is an operation**: it writes the new version, waits
  until every node using it reports that version, then deletes the old one
  ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)). The operation reports the nodes still holding the
  old version, and succeeds only when the old version is gone;
- a remote store is deleted only when no namespace names it; a namespace only
  when no share names it, after which GC drains it ([RFC 9 §7.6](rfc-9-gc.md#7.6%20A%20namespace%20no%20share%20names%20is%20drained)) and the
  namespace's status shows the drain.

GC's two index rebuilds are actions on a namespace,
`POST /namespaces/{id}:rebuild-index` and
`POST /shares/{id}:rebuild-reverse-index`, each taking `check=true` for the
mode that writes nothing; each is an operation whose result lists the
mismatches found ([RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)). The reverse-index rebuild sets GC's hold
before it starts, as RFC 9 requires; the API only starts it.

### 3.4 Adapters

An adapter is configured by settings at installation scope, which a node record
**MAY** override ([RFC 13 §3](rfc-13-configuration.md#3.%20Scopes)); its listen addresses are bootstrap facts of
each host ([RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap)), not records. The resource is
`/adapters/{protocol}`, with `enabled` and the protocol's settings:

- **enable** moves the adapter from configured to serving on every node
  whose effective record enables it; a setting the adapter cannot apply fails
  at configured ([RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states)) and is reported refused by that node;
- **disable** drains and stops it, within the drain deadline;
- a **live** setting reaches new connections; a **restart-class** one builds a
  new adapter through its factory on each node, and only once `New` has
  accepted the settings drains and stops the old one and starts the new; one
  adapter at a time on a node, so a change to SMB never drops an NFS client.
  If the new one fails to start, the last settings that served are built and
  started again and the record is reported refused (§2.3).

The status of an adapter shows, per node, its state and the generation it runs.
A namespace move's release of an address is not an adapter edit: it is RFC 27's
reconfigure, which the node applies as RFC 20 §3.1 describes.

### 3.5 Users, groups, grants and netgroups

These are RFC 16's records with RFC 18's meaning; the API validates and writes
them and decides nothing about identity:

- **create** a user or group with `POST /users` or `POST /groups` and an
  idempotency key (§4.5); **edit** one with `PUT`/`PATCH` and `If-Match`. A
  protocol spelling — uid, gid, SID, Kerberos name — is checked unique in its
  scheme and never defaulted ([RFC 18 §2.3](rfc-18-identity.md#2.3%20IDs%20are%20never%20defaulted%20and%20never%20reused)). A file user created without a
  uid, gid or SID gets one allocated in the create's transaction; an account
  created with a role and no spelling at all is API-only and gets none, so it
  signs in over no file protocol ([RFC 18 §8](rfc-18-identity.md#8.%20Management-API%20users));
- **link** an OpenID Connect subject to a user by writing its `oidc` spelling,
  the (issuer, subject) pair, and **unlink** it by removing it, under the same
  account rules (§5.2): checked unique, allocating nothing, giving no file
  access ([RFC 18 §8.2](rfc-18-identity.md#8.2%20OpenID%20Connect%20sign-in)),
  and audited as a change of the user (§6);
- **a password is write-only**. It is set by `POST /users/{id}:set-password`,
  its hashes derived by identity's `APISignIn.Hashes`
  ([RFC 18 §8.1](rfc-18-identity.md#8.1%20The%20API%27s%20sign-in%20interface)) and sealed as RFC 13 §7
  requires, and never returned. It is refused for a directory principal, which
  signs in only through the directory's own paths
  ([RFC 18 §5.5](rfc-18-identity.md#5.5%20A%20directory%20without%20Active%20Directory%20or%20Kerberos)):
  a second password for one principal drifts from the first;
- **an e-mail address** is an optional field, `email`, of a user: never
  required, never defaulted, and naming no principal to any file check
  ([RFC 18 §2.2](rfc-18-identity.md#2.2%20Spellings)). The API refuses a value
  that is not a syntactically valid address with `ErrInvalid`, stores it as
  given, and refuses to set it on a directory principal, whose address the
  directory supplies. It is returned only to `operator`, `admin` and the user
  itself; a `viewer` reading another user gets the record without it. There is
  no filter or lookup by it;
- **delete** a user or group is an operation: RFC 16's `DeleteUser` and
  `DeleteGroup` mark the record in their first transaction and drop grants,
  memberships and quotas in batches ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)); the `202` comes after
  the mark, from which the principal is admitted nowhere;
- **disable** is a field of the user; it takes effect on every protocol within a
  second ([RFC 18 §7.2](rfc-18-identity.md#7.2%20Local%20changes%20take%20effect%20within%20a%20second)) and on the API at the next call (§5.3);
- **grants** are `PUT /shares/{id}/grants/{principal}` with an access level —
  `If-None-Match: *` for a principal with no grant, `If-Match` to change one
  (§4.4) — `none` included: a `none` grant is stored and blocks the principal, distinct
  from no grant ([RFC 18 §6.4](rfc-18-identity.md#6.4%20Share%20access%20from%20the%20grants)).
  `DELETE /shares/{id}/grants/{principal}` removes the grant, through RFC 16's
  `DeleteGrant`, so the principal falls through to its groups' grants. A grant
  naming a machine principal is refused with `ErrInvalid` (§5.1). Netgroups are put and deleted whole; a client rule naming a
  deleted one fails closed (RFC 16).

### 3.6 Provisioned records

A record a provisioning file declares is **managed** ([RFC 13 §2.4](rfc-13-configuration.md#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)). The
API **MUST** refuse a write to a declared field of a managed record with `409`
`managed`, naming the source, and **MUST** accept the system fields and
create-only changes RFC 13 lists. Each resource's body carries `managed`: the
source name and revision, or absent. A reload is `POST /nodes/{id}:reload-provisioning`.

## 4. The management API

### 4.1 Shape

The API is HTTP with JSON bodies, under one versioned prefix, `/api/v1` (§9).
Every resource has one canonical path, and a resource body has the same four
parts in every resource:

```json
{
  "id": "sh_7Qk2...",
  "generation": 15,
  "spec":   { "name": "builds", "path": "/builds", "namespace": "ns_b4...", "...": "..." },
  "status": { "state": "enabled", "applied": { "nodes": 3, "of": 3 }, "...": "..." },
  "managed": null
}
```

- **`spec`** is what an operator declares, and the only part a write may
  change.
- **`status`** is what the system records — a share's state and namespaces, a
  location's check outcome, per-node application (§2.4). A write that sets a
  status field is refused, as RFC 13 §2.4 refuses a file that declares a system
  field. A listing page reads the node-status records once and computes
  every row's `applied` from them.
- **Paths take IDs, never names.** `/shares/sh_7Qk2…`, not `/shares/builds`. A
  name is found by `GET /shares?name=builds`, which answers a list of zero or
  one.

> decision: IDs only in paths. A path that accepts a name or an ID makes "does
> this exist?" ambiguous — a share named like another's ID — and a rename moves
> the path under a client holding it. The CLI accepts names and resolves them
> with the list filter. Overturned only if a resource ever lacks a stable ID,
> which no record in RFC 16 does.

**Actions** that are not a field change are `POST` on `{resource}:{verb}`, and
this table is all of them:

| Verb | Resource | Role (§5.2) | Operation (§4.6) | Owning RFC |
| --- | --- | --- | --- | --- |
| `quiesce`, `resume` | share | `operator` | no | [RFC 16 §2.3.1](rfc-16-metadata-store.md#2.3.1%20Share%20names%2C%20paths%20and%20state) |
| `rehome` | share | `admin` | yes | [RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace) |
| `rebuild-index` | namespace | `operator` | yes | [RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt) |
| `rebuild-reverse-index` | share | `operator` | yes | [RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt) |
| `raise-format` | installation | `admin` | no | [RFC 13 §5.5](rfc-13-configuration.md#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap) |
| `rotate-signing-key` | installation | `admin` | yes | §5.3 |
| `reload-provisioning` | node | `admin` | yes | [RFC 13 §2.4](rfc-13-configuration.md#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) |
| `set-password` | user | `operator`; `admin` for an account with a role | no | §3.5, [RFC 18 §8](rfc-18-identity.md#8.%20Management-API%20users) |
| `drain` | node | `operator` | yes | [RFC 30 §5.2](rfc-30-kubernetes.md#5.2%20Scale-down%20is%20a%20drain) |
| `decommission` | node | `admin` | no | [RFC 13 §5.5](rfc-13-configuration.md#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap) |
| `declare-lost` | backup location | `admin` | yes | [RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record), [RFC 26 §4.1](rfc-26-catalog-backups.md#4.1%20Interfaces) |
| `cancel` | operation | the role that may start its kind | no | §4.6 |

An operation's `Kind` is an `OpKind`, a closed set read from this table and
§4.6's list: one value per action above that is an operation, and one per
create or delete §4.6 makes one. So `api_operations{kind}` is a closed set
([RFC 25 §2.4](rfc-25-observability.md#2.4%20Labels)), and `operation_running`
keys on the same value.

### 4.2 Reads and listings

- **A read is consistent.** A `GET` reads the metadata store, not a node's
  cache, so a write acknowledged by any node is visible to a read on any node,
  except the node-local reads, which name their node: a node's open connections
  (below) and the event streams (§6).
- **A listing pages by cursor.** `?limit=` (default 100, at most 1,000) and
  `?after=`, an opaque cursor the previous page returned; the page ends with
  `next` when more follow. A cursor resumes from a key, never an offset, so a
  listing of 10⁵ users that sees inserts behind it neither skips nor repeats
  what was there when it started ([RFC 16 §3.1](rfc-16-metadata-store.md#3.1%20Lookups%20by%20ID%2C%20listings%2C%20and%20who%20may%20use%20them)). A listing is not a
  snapshot: a record changed during it appears at its value when its page was
  read.
- **Filters are on indexed fields only** — name, a share's namespace, an
  operation's state, a time range of audit records. A filter that would scan is
  not offered.
- **Secrets are never read.** A secret-bearing field reads as its reference and
  version, never its value ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)).
- **A node's open connections are a node-local read.**
  `GET /nodes/{id}/connections` answers what each adapter's `Connections()`
  lists now — protocol, remote address, session, since
  ([RFC 20 §3.4](rfc-20-adapters.md#3.4%20Interface)) — from that node, not
  from the store, for `viewer`. It is read-only and writes no client or
  principal record.

### 4.3 Writes

| Method | Meaning | Precondition |
| --- | --- | --- |
| `POST /collection` | create | `Idempotency-Key`, except §4.5's credential calls |
| `PUT /resource` | replace the whole `spec` | `If-Match`; `If-None-Match: *` to create a resource whose path the client names |
| `PATCH /resource` | merge patch of `spec` (RFC 7396): a field present is set, `null` resets it to its default | `If-Match` |
| `DELETE /resource` | delete, or start the deletion | `If-Match` and `Idempotency-Key` |
| `POST /resource:verb` | an action | `Idempotency-Key`, except §4.5's credential calls; and `If-Match` when the action changes the resource's `spec` |

**One request is one transaction.** A write validates with the components'
validators ([RFC 13 §6](rfc-13-configuration.md#6.%20Validation)), then commits its records, the generation they
raise, its audit record (§6) and its idempotency record (§4.5) in one
transaction, or none of them. A write that starts long work commits its first
step that way and returns an operation (§4.6). A write **MUST NOT** answer
success before its transaction commits, and **MUST NOT** depend on any call to
a running component.

**Validators are pure and handed in.** Each component exports, from its
settings package, one function per record kind it reads. The function takes the
value and the current values of the settings a cross-setting rule relates it
to, read tracked in the write's transaction
([RFC 13 §6](rfc-13-configuration.md#6.%20Validation)). It imports nothing that
runs. The root hands the API a table of them, keyed by setting prefix, so the
API imports no component. A prefix with no validator is refused `400`.

**Unknown fields are refused**, in requests as in records
([RFC 13 §6](rfc-13-configuration.md#6.%20Validation)): `400`, naming the field.

**API calls are bounded apart from client traffic.** They draw on none of the
data path's pools ([RFC 24](rfc-24-resources.md)). A node runs at most 64 API
calls at once, a fixed value, and answers the next `503` with `Retry-After`, so
a script in a loop cannot take memory or store transactions from file clients.
A long poll (`?wait=`) holds no call slot while it waits: it is parked, and a
node parks at most a fixed number of them (proposed: 256, settled by the
parked count the conformance harness reaches with every CLI of a deployment
running `--wait`), the next answered `503`. Both bounds are fixed values
[RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)
lists.

> ponytail: one node-wide bound of 64, no per-account share. One script can
> hold every slot. Give each account a fair share of the 64 when an operator is
> refused `503` while another account's script runs.

### 4.4 Concurrency: the entity tag is what the write expects

Every resource read returns an `ETag` that is the value RFC 16's write of that
record takes as `expect`
([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)): its
generation ([RFC 13 §2.3](rfc-13-configuration.md#2.3%20A%20record%20is%20versioned)), its version (a grant, an export policy, a
netgroup) or its position (a quota, §2.4). A client compares it only for
equality. A resource built from
several records — a share's info, its state and its namespaces are three
([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)) — takes its tag from the one its `spec` writes; the others
change through actions that name their expected value, as quiesce names the
state it expects.

- **Every `PUT`, `PATCH` and `DELETE` MUST carry `If-Match`.** One without is
  refused `428`. One whose tag is not the current one is refused `412`, code
  `generation_mismatch` (RFC 16's `ErrGenerationMismatch`), with the current
  tag in the body.
- **A resource with no record is created by `POST` on its collection**, or —
  where the client names its path, as a grant, a setting at a scope, a
  netgroup or a quota — by `PUT` with `If-None-Match: *` in place of
  `If-Match`. That `PUT` is refused `412` `generation_mismatch` if a record
  exists, with its tag in the body; so a retried create that had
  committed gets `412`, and the client reads what is there. An absent resource
  has no tag, so `If-Match` on it is `412`.
- **The check is in the transaction.** The write reads the record with conflict
  tracking and compares its tag there ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)), so two writes
  with the same tag on two nodes cannot both commit: one commits and the other
  conflicts, retries, reads the new tag and is refused `412`.
- **A background writer is not overwritten.** The parts of a resource that a
  component changes — a share's state during a cut or removal, its namespaces
  during a re-home — are never in a `spec`, so a stale `PUT` cannot revert them.

> decision: `If-Match` is required, not optional. Optional, every script omits
> it, and the lost update of the Start-here example is the default behaviour.
> The CLI reads before it writes, so the cost falls on nobody who uses it.
> Revisit if a client class appears that cannot read first, and give it a named
> unconditional action, not a missing header.

### 4.5 Idempotency

Every `POST` and every `DELETE` **MUST** carry an `Idempotency-Key`, a client
value unique per intended action, except the credential calls below; one
without is refused `428`. The server
keeps, per (account, key), the digest of the request — method, path, body — and
the answer it gave, written in the request's own transaction:

- a request whose key is recorded with the **same** digest gets the recorded
  answer, status and body included, and changes nothing — `202` and the same
  operation ID for long work;
- the same key with a **different** digest is refused `422`
  `idempotency_mismatch`;
- a request refused before its transaction — validation, a precondition —
  records nothing, so a corrected retry with the same key is a new request;
- records are kept 24 h, then deleted.

**A call that carries or issues a credential keeps no idempotency record.**
Sign-in, refresh, `POST /auth/password`, `:set-password` and API-token creation
**MUST NOT** carry an `Idempotency-Key` — one is refused `400` — and the server
records neither their digest nor their answer, so no token, password or digest
of one is kept anywhere but §5.3's hashes. A retry is a new call:

- a retried **sign-in** opens a second session; the first expires unused;
- a retried **refresh** presents the token the lost answer rotated, so §5.3's
  reuse rule revokes the session, and the client signs in again;
- a retried **password change** is refused, since the current password it
  presents is no longer current; the client signs in with the new one;
- a retried **`:set-password`** sets the same password again;
- a retried **API-token creation** creates a second token. The first, never
  shown, is listed under the account and revocable, and expires as §5.3 bounds.

> decision: credential calls are not idempotent, because replaying their answer
> means storing a usable credential for the record's 24 h, which §5.3's
> hash-only rule forbids. A lost answer costs a sign-in or an unused token, not
> a credential at rest. Revisit only if a client class appears that cannot sign
> in again after a lost refresh, and then give refresh a short reuse grace, not a
> stored answer.

A key is scoped to its account, so two operators cannot collide, and the
record holds the answer, not a reference to a resource that may since have
changed. A client **MUST** reuse a key across its own retries of one action and
**MUST NOT** reuse it for a second action.

> ponytail: an idempotency record per mutating call, kept a day. At an
> operator's call rate that is nothing; a script making 10⁶ calls a day keeps
> 10⁶ small records. Shorten the window, or key it per client instance, when
> `api_idempotency_records_written_total` (§11) shows 10⁶ written per day.

### 4.6 Long-running operations

Work that does not finish in one transaction — a share's removal, a user's
deletion, a clone, a restore, a backup, a re-home, a move, a credential
rotation, a signing-key rotation, an index rebuild, a location's loss — is an **operation**. Its call commits the work's
first durable step and answers `202 Accepted` with `Location:
/api/v1/operations/{id}` and the operation's body.

![A long-running operation: the client's DELETE commits the removal's mark, the operation record and the audit record in one transaction and gets 202 with an operation ID; the owning component drives the steps and records each in its own cursor; the operation's progress is read from that cursor; when the driving node crashes, the work resumes from the cursor where its owner restarts, and a GET on any node keeps reporting it](img/rfc23-operation.svg)

```go
// Operation is the API's record of one piece of long work. It is a view onto
// the owning component's own durable state, plus what the API adds: who asked,
// when, and the answer.
type Operation struct {
	ID       OperationID
	Kind     OpKind     // closed set (§4.1): "share.delete", "user.delete", "share.rehome", "namespace.rebuild-index", ...
	Target   ResourceRef
	Account  Principal  // who started it
	State    OpState    // pending, running, succeeded, failed, cancelling, cancelled
	Progress Progress   // read from the owner's cursor, never estimated by the API
	Started  time.Time  // store time
	Updated  time.Time
	Error    *Problem   // set when failed: the owner's error, as §4.7 carries it
	Result   json.RawMessage // set when succeeded: the new share's ID, the mismatches found, ...
}

type Progress struct {
	Phase string // the owner's step: "mark", "snapshots", "files", "namespace", ...
	Done  int64
	Total int64  // -1 when the owner cannot know it in advance
	Unit  string // "files", "bytes", "shards", "nodes"
}
```

- **The owner drives, not the API.** An operation is driven by the component
  its RFC names — a share's removal by the coordinator RFC 16 §2.3.2 names, a
  re-home by RFC 27's — and resumes as that RFC says after a crash or failover.
  The operation record **MUST NOT** choose a driver for such work. Its progress
  is read from the owner's durable cursor, so a restart loses none of it.
- **Work no component drives on its own** — a credential or signing-key rotation, GC's index
  rebuilds, a provisioning reload — is driven by the operation runner (§2.2) of a
  node, under a claim in the operation record: (node, node epoch,
  expiry), the expiry 30 s ahead and renewed every 10 s, both fixed.
  **(cluster)** A claim that lapses is taken by any node, which resumes from the
  record's last step. Each such kind **MUST** record its steps durably and make
  each idempotent, or it is not an operation.
- **Reading.** `GET /operations/{id}` on any node answers from the store.
  `?wait=30s` returns when the state or phase changes, or after the wait
  (at most 60 s), whichever is first, so a client polls without a loop of its own.
- **Cancel** is `POST /operations/{id}:cancel`, and exists only for a kind whose
  owning RFC defines one — a move's abort before publication
  ([RFC 27 §3.1](rfc-27-namespace-migration.md#3.1%20Interfaces)). Every other kind answers `409` `not_cancellable`: a
  share's removal is one-way, and a re-home has no cancel, only a rate of 0.
- **One per target and kind.** A second request for the same work on the same
  target while one runs is refused `409` `operation_running`, naming it.
- **Ends.** A finished operation is kept 7 days, then deleted; its audit records
  stay for the audit's own retention (§6).

### 4.7 Errors

An error is an `application/problem+json` body (RFC 9457) with these fields:

| Field | Holds |
| --- | --- |
| `status` | the HTTP status |
| `code` | the component's error value, by its name — `bound_setting`, `stale`, `locked`, `moving` — or one of the API's own (below) |
| `title`, `detail` | text for a person, never parsed |
| `field`, `rule` | for a refused value: the field, and the rule it broke, as RFC 13 §6 requires |
| `names` | the things the refusal names: lagging nodes, the backups holding a location, the source of a managed record |
| `retryable` | whether the same request may succeed later unchanged |
| `request_id` | the request's ID, as the audit record and the logs carry it |

**The code is the component's.** A component's closed error set
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values), [RFC 12](rfc-12-snapshots.md#3.1%20Interfaces), [RFC 26](rfc-26-catalog-backups.md#4.1%20Interfaces), [RFC 27](rfc-27-namespace-migration.md#3.1%20Interfaces)) reaches the client by
name, and the API adds only its own: `unauthenticated`, `forbidden`,
`generation_mismatch`, `precondition_required`, `idempotency_mismatch`,
`managed`, `operation_running`, `not_cancellable`, `last_admin`,
`password_change_required`, `starting`, `unavailable`. One table maps every
error to a status, as [RFC 20 §4.5](rfc-20-adapters.md#4.5%20One%20error%20table%20per%20protocol) maps every service error to a wire
status, and a test proves it total (§12):

| Status | When |
| --- | --- |
| `400` | a refused value or unknown field (`ErrInvalid`) |
| `401` | no valid token |
| `403` | the role does not permit the call; a token that may only change its password |
| `404` | no such resource, or one the caller may not know exists |
| `409` | the resource's state refuses it: `ErrStale`, `ErrLocked`, `ErrMoving`, `ErrDelay`, a bound setting, a managed record, an operation running, the last admin |
| `412`, `428` | §4.4 and §4.5 |
| `422` | an idempotency key reused with another request |
| `429` | sign-in throttled (§5.3) |
| `503` | the metadata store unreachable, the node still starting, or its API calls at their bound; with `Retry-After` |

An error the table does not name is answered `500` and counted; any increase is
a bug, as RFC 20 treats an unmapped wire error.

## 5. Authentication and authorization

### 5.1 API accounts are principals with a role

An API account is a `User` record ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)) with one field this RFC
adds: its **role**. RFC 18 §8's rules bind it:

- **A role grants nothing on files.** No role makes an account `admin`,
  privileged or granted on any share; file access comes only from share grants
  ([RFC 18 §6.4](rfc-18-identity.md#6.4%20Share%20access%20from%20the%20grants)).
- **An account and a file user MAY be one record**, sharing a name and
  password; the API reads its role, file access reads its grants.
- **A machine principal is neither.** It holds no role, and setting a share
  grant that names one is refused with `ErrInvalid`
  ([RFC 18 §2.4](rfc-18-identity.md#2.4%20Machine%20principals)): a machine that
  needs file access is linked to a user, and signs in as that user.
- **A service account** — the Kubernetes operator, a backup script — is a
  `User` record with a role, no password, and **no protocol spelling**: no uid,
  gid, SID or Kerberos name. It signs in only with an API token (§5.3), and can
  authenticate to no file protocol, because there is nothing it could present.

So RFC 18's question whether the API needs accounts that are not principals is
answered no: a token-only account is a principal with no spellings, and needs no
second store.

### 5.2 Roles

| Right | `viewer` | `operator` | `admin` |
| --- | --- | --- | --- |
| read every resource but accounts, tokens and the audit | ✓ | ✓ | ✓ |
| health detail, operations, node status, a node's open connections | ✓ | ✓ | ✓ |
| shares, grants, export policies, quotas, snapshots, policies, backups, restores | | ✓ | ✓ |
| remote stores, namespaces, adapters, settings, netgroups, and the secrets they name | | ✓ | ✓ |
| users and groups that hold no role | | ✓ | ✓ |
| accounts with a role, roles, service tokens of others | | | ✓ |
| drain a node | | ✓ | ✓ |
| raise a format version, decommission a node, state a location lost, re-home, move, rotate the signing key | | | ✓ |
| read the audit, and the `access`, `admin` and `logon` event streams | | | ✓ |
| read the `quota` event stream | | ✓ | ✓ |
| own password, own tokens, sign out | ✓ | ✓ | ✓ |

- Authorization is checked by the API, per action, against the account's
  current record — never against a role carried in a token (§5.3).
- **An operator cannot raise privilege.** Giving an account a role, or editing
  a user that holds one, is `admin`'s; so an operator cannot make itself admin by
  editing its own record, nor write `identity.oidc` or `api.oidc_role_map`,
  which map an issuer's claim to a role (§5.5).
- **A secret is written as its owner is.** A write of a `Secret` is authorized
  as a write of the resource that names it — a remote store's credential as the
  store, a backup location's as the location — and no route writes a secret by
  reference alone. An account's password and every hash derived from it ([RFC 18](rfc-18-identity.md#8.%20Management-API%20users)) are written only through
  `:set-password` and `POST /auth/password`, under the account rules here. The
  signing key of §5.3 and the wrapping keys are written by no route; the
  signing key changes only by `POST /installation:rotate-signing-key`.
- **The last admin stays.** Disabling, deleting or demoting the last enabled
  account with role `admin` is refused `409` `last_admin`.
- A caller without the right to read a resource gets `404`, not `403`, for it,
  so an account's existence is not revealed to a `viewer`.

> ponytail: three installation-wide roles, no per-share or per-tenant scoping.
> At 10⁴ shares a tenant's operator can change every tenant's share. Add role
> bindings scoped to a set of shares when a deployment hosts tenants that do not
> trust each other's operators.

### 5.3 Sign-in, tokens and sessions

- **Sign-in** is `POST /api/v1/auth/sign-in` with a name and password. The API
  verifies them by calling identity directly, `APISignIn.VerifyPassword`
  ([RFC 18 §8.1](rfc-18-identity.md#8.1%20The%20API%27s%20sign-in%20interface)), never through `vfs`: the account's password hash is a
  `Secret` sealed under the `storage` wrapping key, which every API node opens
  ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)). It returns an
  **access token** and a **refresh token**. A disabled or deleted account
  ([RFC 18 §3.7](rfc-18-identity.md#3.7%20Disabled%20and%20deleted%20principals)) cannot sign in, and the refusal does not say which.
- **The access token** is a signed JWT (RFC 7519) naming the account's principal,
  the session and an expiry 15 min ahead, a fixed value. It carries no role. On every call the
  API verifies the signature and expiry and then reads the account — through
  `APISignIn.Account`, identity's cache keyed by the principals generation
  ([RFC 18 §7.2](rfc-18-identity.md#7.2%20Local%20changes%20take%20effect%20within%20a%20second)) — and refuses a disabled or deleted account, a
  revoked session, or a token issued before the account's **not-before** time.
  So disabling an account, changing its role, changing its password or signing
  it out everywhere takes effect on every node within a second, not at token
  expiry.
- **The refresh token** is opaque and random, stored only as its hash in a
  session record with an expiry of `api.session_lifetime` (installation, live;
  proposed: 12 h, a working day of one sign-in). Each use returns a
  new refresh token and invalidates the old one; presenting an invalidated one
  revokes the whole session, which is how a stolen token is found.
- **An API token** for a service account is opaque, random, shown once at
  creation, stored as its hash, and **MUST** have an expiry no later than
  `api.token_max_lifetime` (installation, live; proposed: 365 days). It is presented as a bearer token and checked as an
  access token is, with the token record in place of the session.
- **The signing key is installation-wide**: a `Secret` record sealed under the
  `storage` wrapping key, so every node that serves the API verifies a token
  any other issued. It rotates by `POST /installation:rotate-signing-key`,
  `admin`'s, an operation (§4.6), as RFC 13 §7 rotates a secret: the next key
  signs once every node holds it, and the old one verifies until the last token
  it signed has expired. No secret route writes it (§5.2).
- **Throttling.** After 5 consecutive failed sign-ins for an account, or from
  one source address, each further attempt waits twice as long as the last,
  from 1 s up to 5 min, answered `429` with `Retry-After`. A success resets the
  account's count. Every failure is audited (§6). The counts are kept in the
  store, written in the failure's audit transaction, so the bound is 5 per
  account and per source installation-wide, not per node.
- **Transport.** The API **MUST** be served over TLS on every address but
  loopback. Its certificate and key are bootstrap facts of the host.

### 5.4 The bootstrap administrator and the first password

- **Created once, by initialisation.** The explicit initialisation step
  ([RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap)) creates one account, `admin`, with role `admin`. A start
  never creates one, so an installation whose accounts were all deleted is not
  repaired by restarting it (below).
- **It is not a file user.** `admin` **MUST NOT** be given uid 0, any other uid,
  a gid, a SID, a Kerberos name, a group membership or a share grant
  ([RFC 18 §8](rfc-18-identity.md#8.%20Management-API%20users)). It can be given them later by an operator, like any user.
- **The first password.** The step takes the password from a reference — an
  environment variable or a file on the host, as a provisioning file names a
  secret — or, if none is given, generates 24 random bytes, encodes them, and
  prints them once to the step's own standard output, never to a log.
- **It must be changed.** A generated password marks the account
  **must-change**. Until it changes, a sign-in returns a token that permits only
  `POST /auth/password` and sign-out; every other call is refused `403`
  `password_change_required`. The change **MUST** present the current password,
  must-change or not: a stolen first-sign-in token must not set a password on
  its own.
- **Recovery.** Lost admin credentials are reset by an offline command of the
  server binary on a node that is stopped, with the host's bootstrap. It sets a new generated password with must-change, writes
  an audit record naming the host and the command, and nothing else. It is the
  one change not made through the API.

> decision: the break-glass reset bypasses the API, because the API is what is
> locked. It needs the host's bootstrap and the store's credential, which an
> attacker who has those already holds everything. Withdraw it if a deployment
> must prove that no change bypasses the API, and replace it with a second
> admin account kept sealed.

### 5.5 OpenID Connect sign-in

An installation **MAY** let API accounts sign in through an OpenID Connect
issuer it configures (`identity.oidc`). It is for this API only: no file
protocol can carry the token
([RFC 18 §8.2](rfc-18-identity.md#8.2%20OpenID%20Connect%20sign-in)).

- **The API runs the device authorization grant** (RFC 8628) for the client,
  so a headless CLI can sign in. `POST /api/v1/auth/oidc/device` with an
  issuer's name calls that issuer's device authorization endpoint and returns
  the user code, the verification URI, the device code and the poll interval.
  `POST /api/v1/auth/oidc/token` with the device code polls the issuer's token
  endpoint once, answering `202` with `Retry-After` while the user has not
  approved, and honouring `slow_down`. The client holds the device code, as
  RFC 8628's client does; the API holds nothing between polls, so any node
  answers one. A confidential client's secret is a `Secret` sealed under the
  `storage` wrapping key ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)),
  sent only to the issuer's token endpoint, over TLS.
- **Verification is identity's.** The API passes the ID token the issuer
  returned to `APISignIn.VerifyToken` and gets back the linked principal and
  its claims ([RFC 18 §8.1](rfc-18-identity.md#8.1%20The%20API%27s%20sign-in%20interface)). It then
  checks the account as for a password sign-in and issues **its own** access
  and refresh tokens (§5.3). Revocation, hash-only storage and the per-call
  account check are unchanged. The ID token is not stored, and no client
  presents one: the API takes every ID token from the issuer's token endpoint
  itself.
- **Roles from a claim.** `api.oidc_role_map` maps values of the issuer's role
  claim to `viewer`, `operator` or `admin`. The session's role is the higher of
  the account's own and the highest one the claim maps to, recorded in the
  session record, never in a token. A token with no mapped value adds no role
  beyond the account's own; an account left with no role is refused `403`.
  `identity.oidc` and `api.oidc_role_map` are `admin`'s to write, since either
  can grant `admin` (§5.2).
- **Password sign-in per role.** `api.password_sign_in` lists the roles that
  may sign in by password (default: every role). A password sign-in for any
  other role is refused `403` `password_sign_in_disabled`, audited. The last
  enabled `admin` account with no `oidc` link keeps password sign-in whatever
  the setting, so the break-glass reset (§5.4) always leaves a way in.
- **Audit.** Each OpenID Connect sign-in, failed or not, is audited as a
  sign-in (§6), naming the issuer and the principal, never the subject's
  address or user name; a link or unlink is a change of the user.

> ponytail: the claim's role is read at sign-in and fixed for the session's
> life (`api.session_lifetime`); a refresh does not ask the issuer again, so a
> role removed there lasts until the session ends. Re-read it at refresh, with
> the issuer's refresh token kept sealed, when a deployment needs role removal
> faster than a session.

## 6. The audit

Every management action leaves an **audit record**:

| Field | Holds |
| --- | --- |
| time | store time of the commit |
| actor | the account's principal and name, the session or token ID; for the break-glass reset, the host |
| node, source | the node that took the request; the client's address |
| request | method, path, the request ID, the idempotency key |
| target | the resource's kind and ID, and the generation before and after |
| change | the `spec` fields changed, with old and new values; a secret field as "set", never a value |
| outcome | applied, refused with its problem code, or the operation's ID |

- **An applied change and its audit record commit together.** The record is
  written in the change's own transaction (§4.3), so a change that committed
  without its audit record cannot exist, and a record of a change that did not
  commit cannot either. An operation's state changes — started, each phase,
  ended — are audited in the transactions that make them.
- **Refusals are audited too**, in a transaction of their own: every request
  refused after authentication, and every sign-in, failed or not. A request
  refused before authentication is counted, not recorded, except at sign-in,
  where throttling bounds the rate.
- **The audit is append-only.** No API call edits or deletes a record. Records
  older than `audit.retention` (installation, live; proposed: 400 days, a year
  and a quarter, so one annual review finds the whole year) are deleted in
  bounded batches.
- **Reading** is `GET /api/v1/audit`, for `admin`, filtered by time range,
  actor and target, and paged as §4.2 says. Delivery to an outside collector is
  [RFC 25 §4](rfc-25-observability.md#4.%20Event%20streams)'s `admin` event
  stream, fed from these records. The export's position is a cursor record per
  node that takes requests, kept in the metadata store; a lost node's cursor is
  taken over by a live node
  ([RFC 25 §4.3](rfc-25-observability.md#4.3%20From%20the%20emit%20point%20to%20the%20spool)).
- **The event streams** are read at `GET /api/v1/events/{stream}`, filtered by
  share, time and sequence range, from the spool of the node that answers
  ([RFC 25 §4.7](rfc-25-observability.md#4.7%20Export)), with the roles of §5.2. The spool is
  node-local ([RFC 25 §4.3](rfc-25-observability.md#4.3%20From%20the%20emit%20point%20to%20the%20spool)), so this is a node-local read (§4.2); a
  per-node path needs RFC 25 §4.7, which names this one.

This is the audit of the management plane. A file client's access events
([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)) are a separate stream, and what an ACL's audit entries select is
[RFC 19](rfc-19-authorization.md)'s.

## 7. The CLI

The CLI is a client of the management API and of nothing else:

- it **MUST NOT** open the metadata store, a journal or a remote store, or read
  the host's bootstrap ([RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap)). Every command is one or more API calls; a command the
  API cannot serve does not exist;
- the API's wire types are a leaf package that imports nothing of the server,
  and the CLI links no package of the metadata store, the block stores or the
  control-plane server;
- it resolves names to IDs with the list filters (§4.1), reads before it writes
  and echoes the `ETag` it read, opaque, in `If-Match` (§4.4), generates one
  idempotency key per command and reuses it across that command's retries
  (§4.5), and on `412` shows the difference and stops rather than retrying;
- for an operation it prints the operation's ID and, with `--wait`, follows it
  to its end with `?wait=`; interrupted, it can be resumed with
  `operation wait <id>` from any machine;
- `user create` and `user update` take `--email`, and an empty value clears it
  (§3.5);
- `login --oidc <issuer>` signs in through §5.5's device grant: it prints the
  verification URI and user code, polls the API, and stores the API's refresh
  token as a password sign-in's; it needs no browser on the machine it runs on;
- it keeps named contexts — server URL, account, TLS trust — and stores a
  refresh token or an API token with access for its user only;
- **its reference is generated** from its own command definitions, never
  written by hand, and a change to a command regenerates it in the same change.

The server binary's own commands — initialise, start, stop, the break-glass
reset — are not the CLI. They act on the host, with its bootstrap, and are the
only things that do.

The Kubernetes operator ([RFC 30](rfc-30-kubernetes.md#3.2%20DittoCluster)) is a client like the CLI. It
reconciles its cluster resource against the API with a service account. Its
role is `admin`, because a scale-down decommissions node records
([RFC 30 §5.2](rfc-30-kubernetes.md#5.2%20Scale-down%20is%20a%20drain)) — the roles' ponytail (§5.2) is what keeps it from a narrower one — and reads readiness from the endpoints of §2.5
([RFC 30 §4.3](rfc-30-kubernetes.md#4.3%20Health)).

## 8. Cluster-wide behaviour

These rules bind from the single node on, where most of them cost nothing
([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)); the ones marked **(cluster)** bind once there is a second node.

![One API write in a cluster: the CLI sends a PUT to the API on N1; N1 validates with the component's validator, checks If-Match, and commits the record, the settings generation and the audit record in one transaction in the metadata store; every node, N1, N2 and N3, reads the settings generation each second, re-reads the record, applies it by its binding class and writes the position it has reconciled through to its node-status record; a GET on any API node joins those reports into the resource's status](img/rfc23-write-path.svg)

- **Any node answers any call.** The management API is composed on every
  node (§2.1). A client may send any call to any node and gets
  the same answer, because every read and every precondition is evaluated
  against the store, except the node-local reads of §4.2, which name their
  node.
- **Writes go through the metadata store**, as control-plane records. No node forwards a write to a
  "leader": there is none, and the store's transactions order concurrent writes.
- **Application spreads by counter.** A node learns of a change only through
  the three counters of §2.3
  ([RFC 13 §5](rfc-13-configuration.md#5.%20Binding%20classes)), and reports what it applied (§2.4). A live change
  reaches every node within 5 s; a node behind is RFC 25's
  `config_generation_behind` condition, named in the resource's status.
- **(cluster)** **Operations are claimed, not pinned.** Work the operation runner drives is
  claimed by one node and taken over on lapse (§4.6); work a component
  owns follows that component's failover.
- **(cluster)** **A rolling upgrade keeps the API served.** Nodes are upgraded one at a time
  ([RFC 30 §5.1](rfc-30-kubernetes.md#5.1%20Rolling%20upgrade)); a call that needs a newer record schema is
  refused until the schema's active version is raised (§9).

### 8.1 Failure modes

| Failure | What the API answers | What the node does |
| --- | --- | --- |
| metadata store unreachable | `503` `unavailable`, `Retry-After`; health detail names it | keeps the running set; a full reconcile at the next read that works (§2.3) |
| a record fails to build its component | the write committed; status shows the node refused it | stops only what it configures; keeps the old component where one was serving (§2.3) |
| an adapter fails to start | — | reports the record refused and keeps the others serving; readiness as §2.2 sets it |
| a node frozen or partitioned | status names it behind; `config_generation_behind` | — |
| **(cluster)** the runner's node dies | `GET` on any node keeps reporting the last step | another node takes the claim after it expires (§4.6) |
| a stop step misses its bound | — | §2.2's rule: leaves its dependencies open and exits non-zero |
| an OpenID Connect issuer unreachable | a device-grant call or poll answers `503` `unavailable`; a sign-in whose keys are past their grace is refused, never let in ([RFC 18 §8.2](rfc-18-identity.md#8.2%20OpenID%20Connect%20sign-in)) | — |

## 9. Versioning and compatibility

- **One major version in the path.** `/api/v1`. Within a major version a change
  is additive: a new resource, a new optional field, a new action, a new error
  code. Removing or renaming a field, changing its meaning, or making an
  optional field required is a new major version.
- **Two majors overlap.** A new major version is served beside the old one for
  at least one release, and the old one's responses carry `Deprecation` and
  `Sunset` headers until it is removed.
- **A server refuses what it does not know; a client ignores what it does not
  know.** A request field unknown to the server is refused (§4.3), so an old
  server never silently drops a new client's field. A response field unknown to
  the client **MUST** be ignored by it, so a new server never breaks an old
  client.
- **A field is accepted only once every node reads it.** A field stored in a
  record is accepted only while the installation's active settings schema
  version includes it ([RFC 13 §5.5](rfc-13-configuration.md#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)); before the raise, the newer node refuses
  it `400`, naming the version it needs. So during a rolling upgrade no node
  writes a record an older node would refuse.
- **Discovery.** `GET /api/version` answers the major versions served, with no
  authentication and nothing else. `GET /api/v1/nodes` gives each node's binary
  version, for `viewer`. The CLI reads `/api/version` at first contact with a
  context and refuses to run against a server that serves no major version it
  speaks, naming both.

## 10. Invariants

| # | Invariant |
| --- | --- |
| CP1 | `node.New` builds every component; after start, only the node changes which components run, and only from records. |
| CP2 | The API changes nothing but records; it never calls a component to change it, and a write answers success only after its transaction commits. |
| CP3 | Every node applies records by their binding class, in counter order, and reports the position it has reconciled through per kind, the records it refused and those awaiting a restart; a record is applied on a node only when that position reaches the record's own and it is in neither set. |
| CP4 | Stop is the reverse of start; the API outlives the adapters and the store outlives everything. |
| CP5 | Every `PUT`, `PATCH` and `DELETE` carries `If-Match` — or, for a `PUT` that creates, `If-None-Match: *` — checked inside the write's transaction; a write never commits over a tag, or over a record, its author did not read. |
| CP6 | A request with a recorded idempotency key is answered from the record and applied at most once. |
| CP7 | Long work is an operation whose progress is the owning component's durable cursor; it survives a crash, and is readable from every API node. |
| CP8 | Every error reaches the client as the component's own error by name, and every error has a status in the API's table. |
| CP9 | No API role grants any file access, and the bootstrap administrator has no uid, gid, SID, Kerberos name or grant unless an operator gives it one. |
| CP10 | Authorization reads the account's current record on every call; a disabled account, a changed role or a revoked session takes effect within a second on every node. |
| CP11 | No secret value appears in an API answer, an audit record, an idempotency record, a log or a metric. |
| CP12 | Every applied management change commits with its audit record; no audit record is edited or deleted by the API. |
| CP13 | The unauthenticated health endpoints answer one word and nothing else. |
| CP14 | The CLI reaches the installation only through the API. |
| CP15 | No request field is accepted before every node can read the record that holds it. |
| CP16 | No package under the control plane imports an adapter, a sidecar or a component's implementation; each is reached through an interface it declares or a table the node hands it. Only `node`, the composition root, imports them, and only the server's start command and the test harness import `node`. |

## 11. Observability

Metric names are shown without the `dittofs_` prefix the exporter adds; names,
labels and histogram buckets follow
[RFC 25 §2](rfc-25-observability.md#2.%20Metrics).

| Answers | Metric | Type |
| --- | --- | --- |
| How many API calls, by call and outcome? | `api_requests_total{op, outcome}` — `op` the route and method from §4.1's resources and Actions table, `outcome` the problem `code` or `ok` | counter |
| How long do they take? | `api_request_seconds{op}` | histogram (`request`) |
| How many calls run now, against the bound of §4.3? | `api_calls_in_flight`, `api_calls_in_flight_limit` | gauge |
| How often is authentication refused, and why? | `api_auth_failures_total{reason}` | counter |
| Sign-ins by method and result | `api_sign_ins_total{method, result}` — `method` `password`, `oidc`, `token`; `result` `ok` or the problem `code`; no issuer subject, address or name as a label | counter |
| How often are writes refused on a precondition? | `api_precondition_failures_total{kind}` — `generation`, `idempotency`, `missing` | counter |
| How often are retries answered from an idempotency record? | `api_idempotent_replays_total` | counter |
| How many idempotency records are written, and deleted at expiry? | `api_idempotency_records_written_total`, `api_idempotency_records_deleted_total` | counter |
| How many operations, by kind and state? | `api_operations{kind, state}` | gauge |
| How long has the oldest running operation of each kind been running? | `api_operation_oldest_seconds{kind}` | gauge |
| How often is an error answered with no row? | `api_unmapped_errors_total` | counter |
| What has each node reconciled through? | `node_reconciled_position{kind}` — `settings`, `shares`, `principals` | gauge |
| How often does a reconcile step fail, by what it applies? | `node_reconcile_errors_total{kind}` | counter |
| How many records await a restart on this node? | `node_restart_pending` | gauge |
| Audit records written, by outcome | `audit_records_total{outcome}` | counter |

`api_unmapped_errors_total` is zero on a healthy node. Records refused by
validation are RFC 13's `config_refused_total`, not counted again here. A node
behind is RFC 25's `config_generation_behind` condition
([RFC 25 §3.9](rfc-25-observability.md#3.9%20The%20condition%20registry)), fed by `node_reconciled_position`.

**Spans.** One span per API call, named by its `op`, its attributes the
resource kind and ID and the outcome, never a body field
([RFC 25 §6.1](rfc-25-observability.md#6.1%20Spans)). An operation's work opens a root span of its own that
*links* to the request span that started it.

## 12. Conformance

Conventions are the index's ([Test tiers](rfc-index.md#Test%20tiers)). The tiers for the node and what it builds:

| Tier | What runs |
| --- | --- |
| unit | `*vfs.Server` through `vfs.New`, with a fake engine and in-memory metadata, and no node |
| unit | the gateway through `vfs.NewGateway`, with a fake `Router` and `Transport` |
| integration | `*vfs.Server` with the real engine and in-memory stores, without the node |
| multi-node | two `node.New` over one in-memory control-plane store and a loopback `Transport`, driven by writes to the store, with fault injection: a lost reply, a duplicate send, a stale epoch |
| e2e | the adapters |

Each check below runs, through the API, against the node `node.New`
builds, unless marked **(cluster)**, which runs at the multi-node tier. No check
builds a node any other way, and an architecture test fails on a caller of
`node.New` other than the server's start command and the test harness.

| Requirement | Check | Catches |
| --- | --- | --- |
| §2.1 API writes only records | Fault the metadata store's commit of a share create; assert `503`, no share context exists, and none appears after the store recovers and a full reconcile runs. Then let the commit succeed and kill the node before it reads the share list; restart: assert the share serves. | a handler calling the engine directly |
| §2.2 order | Record each component's start and stop. Assert the order of §2.2, that the health endpoints answered `starting` before step 5 ended, that a refused registration started no component, that no adapter listened before the first reconcile completed, that **(cluster)** the shards were handed over before `vfs.Serve` stopped, and that an operation readable before stop is readable through the API until step 3. | an API that stops before the drain it reports |
| §2.3 reconcile | Change a live share setting while the store is unreachable to the node's reads but not its writes; restore reads: assert the change applies at the next read, not at the next generation move. | a missed generation lost for good |
| §2.4 applied | Freeze one node's reconcile after a share setting changes. Assert the resource's status names that node as behind and, after 5 s, a health condition names it. Change a setting whose own generation is 2 while the settings generation stands at 100, and freeze a node at 100: assert the setting reads not applied there. Change a node-scoped restart-class setting, then a live one: assert the first reads not applied and the second applied on that node until it restarts. | "applied" inferred from the writing node, from a record's own generation, or past a pending restart |
| §2.5 health | Call the unauthenticated endpoints during start, serving, drain and stop: assert `live` from the live endpoint and one of the four readiness words from the ready one, and nothing else, and that `GET /api/v1/health` without a token is `401`. Assert the metrics listener answers without a token. | grace state or names leaked to anonymous callers |
| §3.2 create admits nobody | Create a share; mount it as alice over SMB and NFS: refused. Grant alice; mount: admitted. | an open default |
| §3.2 delete | Delete a share with 10⁵ files; kill the node during phase "files" once `Done` > 0; restart: assert the same operation ID reports `running`, that `Done` is at least that value and rises, and that it ends `succeeded`. Delete again with a new key: `409` `stale`. | progress the API made up |
| §3.5 grants | Grant `Finance` read on a share and alice, a member, `none`: assert alice refused. `DELETE` alice's grant: assert her grant gone and alice granted read through `Finance`. `PUT` a grant naming a machine principal: assert `400` `ErrInvalid` and no grant written. | `none` stored as no grant, or a machine granted file access |
| §4.4 lost update | Two clients read a policy, both `PUT` with the same `If-Match`, sent to two API nodes at once **(cluster)** and on one node in a tight loop: assert exactly one commits and the other is `412`. Send a `PUT` without `If-Match`: `428`. Two clients `PUT` the first grant of one principal with `If-None-Match: *`: assert one commits and the other is `412`. | the Start-here lost update; a create overwriting a concurrent one |
| §4.4 background field | Start a re-home of a share; `PUT` its spec with a stale copy that includes its old namespace: assert the namespace is unchanged and the `PUT` refused. | a `PUT` reverting a component's write |
| §4.5 idempotency | Send a `DELETE` with a key, drop the answer, resend: assert one operation, the same ID and body. Resend with the same key and another body: `422`. Send a create that fails validation, fix it and resend with the same key: accepted. Sign in, refresh and create an API token, each twice; read the store's raw keys: assert no idempotency record exists for them and no token value or password appears; assert the second refresh revoked the session. | double application on retry; a credential kept in a replay record |
| §4.6 runner claim | **(cluster)** Start a credential rotation; kill the node that claims it; assert another node claims it after the claim expires and it completes, deleting the old version once. | an operation lost with its node |
| §4.7 error table | For every error value in every closed set, provoke it through the API, or inject it at the seam where none can be provoked; assert each has a status and code, and `api_unmapped_errors_total` stays 0. | an error with no row |
| §5.1 roles grant nothing | With the bootstrap admin and every role, mount every share over every flavour: assert every mount refused. Read the admin's record: no uid, gid, SID, Kerberos name or grant. | an API role leaking into file access ([RFC 18 §11](rfc-18-identity.md#11.%20Conformance)) |
| §5.2 roles | Read the route list from the router and compare it with §4.1's resources and Actions table; a route in neither fails the check. For every route, call it as `viewer`, `operator` and `admin`; assert the table of §5.2, `404` where the caller may not read. As an operator, give yourself `admin`: refused. As an operator, write the password secret of an admin account, and the signing key, through every secret-bearing route: refused, and neither changed. Demote the last admin: `409` `last_admin`. | a route missing its check; privileged credentials replaced through a secret write |
| §5.3 revocation | Sign in on two nodes; disable the account; assert every call on both is refused within 1 s, with the access token unexpired. Reuse an already-rotated refresh token: assert the session is revoked. | a token outliving its account |
| §5.5 OpenID Connect | Against a test issuer, run `login --oidc` with no browser: assert a session whose tokens are the API's, and no ID token stored. Map a claim value to `operator` for an account whose own role is `viewer`: assert `operator` rights, and `viewer` only in a session whose token lacks the value. Set `api.password_sign_in` to `viewer`: assert an `operator`'s password sign-in refused `403`, and the last unlinked `admin` still signs in by password. As `operator`, write `api.oidc_role_map`: refused. | an ID token used as the session; a role carried in a token; an operator raising its own role |
| §5.4 first password | Initialise without a password; assert it is printed by the step and appears in no log. Sign in: every call but the password change is `403`. Change it without the current password: refused. | a first-login token setting a password alone |
| §6 audit | Run every mutating route once, applied and refused. Assert one audit record each, with generations before and after, and no secret value; fault the commit of one change and assert neither the change nor its record exists. | a change with no record |
| §7 CLI | Run the CLI's whole suite with the host's bootstrap and the store unreadable to the CLI's user: assert every command still works. Assert the generated reference matches the command tree. Assert `go list -deps` of the CLI names no package of the metadata store, the block stores or the control-plane server. | a command that bypasses the API |
| §9 schema gate | **(cluster)** With one node not upgraded, send a field the new schema adds: refused `400` naming the version. Raise the version after upgrading it: accepted. | a record an old node refuses |

## 13. Open questions

1. **Which counter a position is of.** [RFC 16](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)
   defines the records this RFC adds and their keys, a `User`'s role and
   not-before time, every record's `Position`, and `expect` on every write,
   `SetGrant` and `SetQuota` included. One question is left: which counter
   (`CFGGEN`, `SL` or `PGEN`) a grant's, an export policy's, a netgroup's and a
   quota's `Position` is of. [RFC 18 §7.2](rfc-18-identity.md#7.2%20Local%20changes%20take%20effect%20within%20a%20second)
   keeps grants and export policies off the principals generation.
2. **Audit volume at scale.** An audit record per change is nothing for
   operators; a provisioning reload that rewrites 10⁴ shares writes 10⁴ records
   in its transactions. Whether a reload's records are summarised per record
   kind is unmeasured.
3. **Server-sent changes.** Clients poll operations and resources. A watch
   (a stream of changes from a generation) would serve the Kubernetes operator
   better; whether it is worth a second read path is open.
4. **Lifecycles the owning RFCs have yet to state.** §2.1 places, but does not
   specify, what this RFC cannot state for another: the construction and
   lifecycle of namespace metadata (RFC 14; open state's is
   [RFC 14 §11.2](rfc-14-open-state.md#11.2%20Lifecycle)'s), the identity
   resolver and authenticator, and whether they hold directory connections or
   keytabs to close (RFC 18), the event sink and the admin-stream writer
   (RFC 25), the pools (RFC 24), the router and the client-address agent
   (RFC 15), shard placement (RFC 11) and the metadata store's close (RFC 16).
5. **The cost of a full reconcile.** A full reconcile after every failed read
   re-lists every record of every kind; during a flapping store, at 10⁴ shares,
   that is once a second. Whether it needs a rate bound, and whether reconciling
   only the records above the reconciled position needs an index by (kind,
   position) in RFC 16, is unmeasured.

## Appendix A — prior art

*Non-normative.*

- **The Kubernetes API** — the model for `spec` and `status`, for
  `metadata.generation` and `resourceVersion` as an optimistic-concurrency tag,
  for list pagination by continue token, and for controllers that reconcile
  desired against running state rather than taking orders. Its API priority and
  fairness, and its audit policy with request and response stages, are the
  reference for §6.
- **Google's API improvement proposals** — AIP-151 for long-running operations
  with metadata and a final result, AIP-155 for request IDs that make a create
  idempotent, AIP-136 for custom methods as `resource:verb`, AIP-158 for
  page-token pagination.
- **Stripe's idempotency keys** — a key per intended action, the stored
  response replayed, a mismatched body refused, kept 24 h.
- **HTTP conditional requests** (RFC 9110 §13) — `ETag`, `If-Match` and `412`;
  `428 Precondition Required` (RFC 6585) for a server that requires them.
  Problem details (RFC 9457) for errors; JSON merge patch (RFC 7396) for
  `PATCH`; `Deprecation` and `Sunset` headers for retiring a version.
- **HashiCorp Vault** — short-lived tokens, renewable leases, and root token
  generation as a break-glass act separate from the API's normal paths.
- **Ceph's manager and TrueNAS's middleware** — storage systems whose CLI and
  web UI are clients of one management API, with jobs for long work; TrueNAS's
  jobs keep progress and are listed after the request that made them.

## Appendix B — where the current code differs

*Non-normative.* The code on the branch this RFC was written against, by section.

| § | This RFC | The code today |
| --- | --- | --- |
| §2.1 | one composition root, `node.New` | the start command composes the node; a second, partial root in `pkg/controlplane/controlplane.go` builds store, runtime and API only and has no importer; the adapter and NFSv3 handler test fixtures build their own |
| §2.1, RFC 20 §3.4 | adapters hold the filesystem service only; the node drives them | adapters are handed the whole runtime by `SetRuntime(rt any)`, and 22 adapter files (16 internal, 6 public) import the runtime; the runtime knows protocols by name (`GetNFSSettings`, `GetSMBSettings`, `SetNFSExportPolicy`, an NFS mount tracker, an SMB oplock-breaker hook) |
| §2.1 | the node exports `New`, `Start` and `Stop` | `*Runtime` exports 156 methods, in 23 of the package's 35 files, and hosts GC, the snapshot scheduler, trash and integrity scans, which are their components' own |
| §2.1, §2.3 | the API writes records, the node applies them | handlers write the database and then call the runtime directly — `AddShare`, `UpdateShare`, `RebindShareBlockStore`, adapter start and stop; a runtime failure after the write is logged as a warning and the request still succeeds (a share whose `AddShare` fails returns `201` and loads at the next restart); share create builds the runtime's share configuration field by field in the handler |
| §2.2 | start: store, content, the server, API, adapters; stop the reverse with the API after adapters | start opens the control-plane database, seeds the admin, default groups and default adapters, opens metadata stores, loads shares, then serves; inside `Serve` the settings watcher and adapters start before the API; shutdown stops adapters, closes stores and then the API |
| §2.3, §8 | every node reconciles by generation, once a second | changes are applied in-process by direct calls and change callbacks; only adapter settings are polled, every 10 s; there are no node records, so a second node sharing the database sees shares, adapters and users only after a restart |
| §2.4 | nodes report what they applied | no per-node report; nothing records whether a change took effect |
| §2.5 | anonymous health answers one word | `/health` and `/health/ready` are unauthenticated and return control-plane database status, share and adapter counts and NFS grace state; the grace route under `/api/v1` with the same data is admin-only |
| §3.1, RFC 16 §2.3 | records in the metadata store | the control plane is a separate SQL database (SQLite by default, or PostgreSQL) through an ORM, beside Badger metadata stores whose configurations it holds; RFC 16 already moves it into the one store |
| §3.2 | create admits nobody; quiesce; rename; delete is an operation | create writes default NFS and SMB adapter configurations and no grant; enable and disable flip a flag and drop sessions, with no `quiesced` state; rename is not supported; delete is synchronous — it removes the snapshots directory, closes the block store and deletes the rows — and purges no metadata or block data |
| §3.3 | rotation and rebuilds are operations | store configurations are edited in place with no bound-field check (RFC 13 Appendix A, C2); GC runs as a job in an in-memory registry lost at restart |
| §3.4 | adapters change by record | adapter create, update and delete start, restart and stop the adapter from the handler; `EnableAdapter` and `DisableAdapter` exist on the runtime with no caller; there is no drain call |
| §4.1 | IDs in paths | shares and users are addressed by name |
| §4.2 | cursor pagination | no listing is paginated |
| §4.4 | `If-Match` required | no entity tags; `PUT` is a partial update through pointer fields; adapter settings carry a version counter used only by the watcher; the share NFS configuration `PATCH` serialises on a process mutex |
| §4.5 | idempotency keys | none |
| §4.6 | operations | GC and warm return `202` with an in-memory job ID; snapshot create returns `202` with `Location`; every other long call — restore, trash empty, share delete, upload drain — blocks the request |
| §4.7 | problem details everywhere | the handlers answer problem details with `code` and `hint`; the authentication middleware answers plain text |
| §5.1, §5.4 | the bootstrap admin is not a file user | the bootstrap admin is created with uid 0 "for permission bypass" and is added to an `admins` group |
| §5.2 | `viewer`, `operator`, `admin`, checked against the record | roles `admin`, `operator`, `user`, carried in the token; `operator` can only list adapters |
| §5.3 | tokens checked against the account each call; installation-wide signing key | the access token is checked for signature and expiry only, so disabling a user or changing a role takes up to 15 min; refresh tokens last 7 days and re-check `Enabled`; the signing secret is per process, from the environment or the host file, and must be copied between nodes by hand (RFC 13 Appendix A, C6); no throttling of sign-in |
| §5.3, CP11 | no secret is read | store configurations are redacted, identity-provider secrets are write-only, but the settings routes return raw values, and the NETLOGON machine secret is kept as a setting |
| §5.4 | first password printed once by initialisation; current password always required | the admin is seeded at start from an environment variable, a configured hash, or a random 24-character password printed only when standard output is a terminal; while must-change is set, the password change does not ask for the current password |
| §6 | an audit record per action, in its transaction | none; nine adapter-settings handlers write an informational log line naming the user |
| §7 | the CLI only calls the API | `dfsctl` is a REST client over `pkg/apiclient`, with contexts in `~/.config/dfsctl/config.json` (mode 0600, access and refresh tokens); `share mount` and `unmount` run the host's mount tools, which is client-side and allowed; `docs/guide/cli.md` is generated by `cmd/gendocs` from both command trees |
| §9 | version discovery and schema gate | only the `/api/v1` prefix; no version endpoint, header or client check; `dfsctl version` is local |
| RFC 13 §2.4 | provisioning file | none; the start seeds the admin, default groups and adapters, and seeds identity providers from the host file when no row exists |
