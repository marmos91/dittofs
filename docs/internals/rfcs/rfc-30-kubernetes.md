---
rfc: 30
title: "RFC 30 — deployment on Kubernetes"
component: kubernetes operator
status: draft
depends_on:
  - "[[rfc-10-journal-replication]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
aliases:
  - RFC 30
tags:
  - rfc
---
# RFC 30 — deployment on Kubernetes

**Status:** draft. Nothing here is implemented; the operator today manages one
server. [§11](#11.%20Open%20questions) lists what is undecided.
**Audience:** anyone changing the operator in `k8s/dittofs-operator`, or deploying
a cluster on Kubernetes.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This is the one platform RFC: it names Kubernetes
objects, and maps onto them rules the core RFCs state without a platform.
Where this RFC and a core RFC differ, the core RFC wins.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

Every DittoFS node is the same: each takes client calls and each keeps some of
the notebooks ([RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20node%20type)). The
pod a client reaches is its receptionist: if another pod is in charge of the
file, it passes the call there and decides nothing itself
([RFC 15 §2.3](rfc-15-topology.md#2.3%20The%20gateway%20forwards%2C%20the%20filesystem%20decides)).
Each pod has its own fast disk and a name that must survive a restart, so the
cluster is one StatefulSet. The replicated metadata store is the town hall, run
by its own operator. The DittoFS operator is the building manager: it adds pods
slowly, and never sends one home before handing its work to another. Where the
picture breaks: a pod's notebook is copied to two others, so losing one pod
unannounced loses nothing — it only costs a short pause.

```text
 clients ──► Service VIP or floating IPs
                │
   StatefulSet: identical pods (local NVMe journal, stable node ID)
                │  node channel between pods: gRPC over mTLS
        ┌───────┴─────────────────┐
        │                         │
 metadata store (own operator)   bucket (outside the cluster)
```

### Scenarios

**Monday morning, connections triple.**
1. New connections spread over the existing pods; each pod's gateway forwards
   what it is not primary for ([RFC 15 §2.3](rfc-15-topology.md#2.3%20The%20gateway%20forwards%2C%20the%20filesystem%20decides)).
2. Nothing scales: adding a pod moves shards, so the pool grows only on
   capacity signals ([§6.1](#6.1%20The%20node%20pool)).

Outcome: no pod added, no shard moved.

**A host dies at 10:00.**
1. The pod and its local volume are gone with the host.
2. Its node lease lapses; replicas take its shards over in about 14 s
   ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)),
   and its clients reconnect through the address to another pod ([§7](#7.%20Client%20addressing)).
3. The StatefulSet cannot reschedule the pod: its volume is bound to the dead
   host. The operator reports the node lost and waits
   ([§5.3](#5.3%20A%20lost%20pod)).
4. Repair restores each shard's replica count on the surviving pods
   ([RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair)).

Outcome: one failover per shard; no operator action needed to stay writable.

**The journals are filling up.**
1. The scaling controller sees journal fill above its threshold on most pods
   ([§6.1](#6.1%20The%20node%20pool)).
2. It raises the StatefulSet by one; the new pod joins with a fresh node ID.
3. The slot table is recomputed and about 1/N of the shards move to it,
   at a paced rate ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)).
4. The controller waits at least the cooldown before scaling again.

Outcome: one node added, a fraction of shards moved, no bucket bytes copied.

**Removing a pod for good.**
1. An operator lowers the pool size by one.
2. The operator marks the highest-ordinal pod draining; it leaves the slot table
   ([§5.2](#5.2%20Scale-down%20is%20a%20drain)).
3. Its clients are moved and its shards handed over, with no grace period.
4. Its node record is decommissioned; only then are the pod and its volume
   deleted.

Outcome: the cluster shrinks without a single failover.

**A node-pool upgrade.**
1. The cloud drains hosts one at a time.
2. The PodDisruptionBudget lets one pod go at a time
   ([§4.1](#4.1%20One%20StatefulSet)).
3. Each evicted pod moves its clients and hands its shards over before it stops
   ([§5.1](#5.1%20Rolling%20upgrade)).

Outcome: shards move ahead of the upgrade instead of failing over behind it.

### How the rest is organised

§1 is the purpose, §2 what the operator has today and what changes, §3 the
resources, §4 the mapping from nodes to Kubernetes objects, §5 lifecycle, §6
scaling, §7 client addressing, §8 secrets and the node channel, §9
invariants, §10 tests and §11 open questions.

## 1. Purpose

The core RFCs say what a node runs and what it must guarantee
([RFC 15](rfc-15-topology.md)). This document answers:

> **Which Kubernetes objects carry the nodes, how does the operator change
> their number, and what must it do before a pod goes away?**

### 1.1 Non-goals

This document **MUST NOT**:

- change any core rule: fencing, placement and failover stay in RFC 10,
  11, 14 and 15;
- specify the metadata store's deployment. It runs under its own operator,
  and this RFC only references it;
- depend on a particular cloud. Every cloud-specific piece is an option behind
  one of the two addressing modes ([§7](#7.%20Client%20addressing)).
- specify a CSI driver. What one needs from the core is there: a volume is a
  share or a project inside one, made by `SetProject` on the volume's empty
  directory and limited by a quota on it ([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)); `statfs` inside
  the volume reports its own room, and a write past its quota is answered as
  a full disk ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)). `SetProject` is safe to retry, so a driver
  that puts the share, the `ProjectID` and the path in its opaque volume ID
  keeps no mapping of its own; deleting a volume is removing its tree, then
  deleting its quota, which ends the project's last record.

## 2. What exists and what changes

**Today.** The operator in `k8s/dittofs-operator` manages one `DittoServer`
resource: a StatefulSet of at most one pod (`replicas` is 0 or 1), a
ConfigMap, persistent volumes, and three Services — file (NFS and SMB), API
and metrics. An optional integration provisions a SQL database; the SQL
metadata stores are being removed, so that integration goes with them.

**What changes.**

| Today | After |
| --- | --- |
| `DittoServer`, one pod, both roles | kept unchanged for a single node ([§3.1](#3.1%20DittoServer%20stays%20the%20single%20node)) |
| — | `DittoCluster`: one pool of identical pods, with size, resources and placement, and a reference to the metadata store ([§3.2](#3.2%20DittoCluster)) |
| one StatefulSet of at most one pod | one StatefulSet of many ([§4](#4.%20Nodes%20to%20objects)) |
| pod deleted on scale-down | pods drained, then deleted ([§5.2](#5.2%20Scale-down%20is%20a%20drain)) |
| no autoscaling | a scaling controller on capacity signals ([§6](#6.%20Scaling)) |
| one Service address | a Service VIP or operator-managed floating IPs ([§7](#7.%20Client%20addressing)) |

## 3. Resources

### 3.1 DittoServer stays the single node

A `DittoServer` **MUST** keep producing one pod with both roles and an embedded
metadata store. The operator **MUST** refuse a `DittoServer` with more than one
replica, as the server itself refuses an embedded store on more than one node
([RFC 15 §2.2](rfc-15-topology.md#2.2%20Startup%20refuses%20what%20cannot%20work)).

### 3.2 DittoCluster

A `DittoCluster` declares:

| Field | Means |
| --- | --- |
| `metadataStore` | how to reach the replicated metadata store ([RFC 16](rfc-16-metadata-store.md)): an address and a credential Secret. The store's own resource, if any, is referenced, never owned |
| `bucket` | the remote tier's endpoint and a credential Secret, mounted on every pod ([§8](#8.%20Secrets%20and%20the%20node%20channel)) |
| `nodes` | pool size, journal volume class and size, resources, placement, the failure-domain label ([§4.1](#4.1%20One%20StatefulSet)), and the scaling policy ([§6.1](#6.1%20The%20node%20pool)) |
| `addressing` | `service` or `floatingIPs`, with their settings ([§7](#7.%20Client%20addressing)) |

Installation settings — lease, dwell, replica count and floor — are not fields
here: they live in the store as [RFC 13](rfc-13-configuration.md) settings. The resource holds only
what Kubernetes must know to place pods.

> ponytail: a cluster is one StatefulSet of identical pods. Split it into a
> protocol Deployment and a storage StatefulSet only when the role split of
> [RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20node%20type) is taken.

**Status** reports the pool's ready count, the cluster's health
([§4.3](#4.3%20Health)), and each share health condition the cluster reports, as a
condition on the resource.

## 4. Nodes to objects

### 4.1 One StatefulSet

The pool **MUST** be one StatefulSet with at least as many pods as the replica
count, every pod the same node image running every component
([RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20node%20type)):

- **Journal volumes.** Each pod has a volume for its journals, from a class the
  resource names; local NVMe is the expected class. A pod's node identity lives
  on that volume (§4.2), so the volume is never reattached to another pod.
- **Failure domains.** Pods spread by the label the resource names — host,
  rack or zone — and the operator **MUST** set each node's failure-domain
  setting from that label, so the slot table's domain rule
  ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)) and Kubernetes placement agree. At most one pod
  per host.
- **Disruption budget.** A PodDisruptionBudget with `maxUnavailable: 1`, so a
  voluntary eviction never takes two members of one shard's replica set at
  once.
- **Pod management.** `podManagementPolicy: Parallel`, since pods find each
  other through the store, not through start order.

### 4.2 A pod's identity is its volume

A node's ID and its node credential ([RFC 15 §4.4](rfc-15-topology.md#4.4%20Node%20channels)) **MUST** be
issued once, when its volume is first initialised, and kept on that volume. A
pod that restarts on its volume is the same node with a new node epoch; a pod
given a fresh volume is a new node.

> decision: identity follows the volume, not the pod name. A StatefulSet
> reuses ordinals, so a name-derived identity would make a replacement pod with
> an empty journal claim to be the lost node and its acknowledged content.
> Overturned only if the journal moves off node-local volumes.

### 4.3 Health

The resource's `Ready` condition **MUST** be false while the pool has no live
pod or the metadata store is unreachable. Readiness probes gate traffic: a pod
is ready once it has rejoined the store and its adapters accept clients, which
they do only after everything under them serves
([RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop)). A shard it cannot serve
is that shard's `shard_relinquished` or `replication_under_floor` condition,
which §5.1's upgrade waits on beside readiness, not a reason to be unready.

Liveness and readiness probes **MUST** be HTTP, on `/health/live` and
`/health/ready`, never TCP on 2049 or 445: a TCP probe on a protocol port opens
a connection the adapter charges and logs
([RFC 20 §3.7](rfc-20-adapters.md#3.7%20Health%20and%20probes)).

The probes are RFC 23 §2.5's endpoints, unauthenticated and answering one word:
the liveness probe is `GET /health/live` and the readiness probe
`GET /health/ready`, and they answer different questions. Liveness fails only
when RFC 25 §3.5's watchdog does — an internal loop has stopped turning, which
a restart may clear — and reads no dependency, so the metadata store, the
remote tier or a journal device failing never restarts a pod. Readiness may
fail on a dependency, the metadata store lost among them (RFC 25 §3.6), and
then takes the pod out of traffic without restarting it.

The operator calls the management API as a service account (RFC 23 §5.1): a
user record with role `admin`, no password and no protocol spelling, signing in
with an API token. It needs `admin` because draining is not all it does: it
decommissions node records (§5.3, §5.4), which RFC 23 §5.2 gives to `admin`
only. The role grants nothing on
files.

## 5. Lifecycle

### 5.1 Rolling upgrade

Pods **MUST** be upgraded one at a time, each draining its clients (§5.2 step 1) and handing its shards over
before it stops ([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)) and the next waiting until the upgraded pod is
ready and has no shard with the `replication_under_floor` or `shard_relinquished` condition (RFC 25 §3.9). The operator runs this itself
(`updateStrategy: OnDelete`) rather than the StatefulSet's rolling update,
which waits for readiness but not for handovers. A pod's `preStop`
hook **MUST** run the same drain and handover, so an eviction the operator did not start
— a node-pool upgrade — moves shards ahead of the stop rather than failing them
over after it. Its termination grace **MUST** cover the client drain and the handover bound.

> ponytail: a handover bounded by the termination grace can be cut short on a
> shard with a large un-offloaded tail, and then becomes a failover, which loses
> nothing but costs a grace period. Size the grace from the handover benchmark
> ([RFC 15 §9](rfc-15-topology.md#9.%20Conformance%20and%20benchmarks)) when real tails make cut-short handovers show up.

### 5.2 Scale-down is a drain

Lowering the pool **MUST** run, for the pod being removed:

1. drain its clients: tell SMB clients through Witness and NFSv4 clients
   through `fs_locations` where to go
   ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)), stop
   accepting new connections, and wait up to the termination grace for
   connections to leave; NFSv3 clients and those left at the deadline reconnect
   through the address ([§7](#7.%20Client%20addressing)), and an NFSv4.1
   session on the pod ends with it ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope));
2. mark the node draining, which takes it out of the placement set and
   recomputes the slot table ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards));
3. hand each shard it is primary of over, and replace it as replica, under the
   ordinary rules and pace ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries));
4. once it is primary and replica of nothing, decommission its node record;
5. only then delete the pod and its volume.

The operator **MUST NOT** delete a pod or its volume before step 4. A
scale-down that would leave fewer pods than the replica count, or fewer failure
domains than the slot table needs, **MUST** be refused.

### 5.3 A lost pod

A pod whose host is lost is a failover, not a scale-down: its shards
fail over ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)), its clients move by address ([§7](#7.%20Client%20addressing)), and repair restores the shards' counts
([RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair)). The pod cannot be rescheduled while its node-local volume is
bound to the lost host. The operator **MUST NOT** release that volume on its own:
the host may return within the re-placement delay, with its journal. It reports
the node lost, and an operator's acknowledgement — or the host's return —
decides. After acknowledgement the operator decommissions the node record,
deletes the volume claim, and the StatefulSet creates a pod with a new volume,
which is a new node (§4.2).

> decision: releasing a lost pod's volume waits for an operator. Released
> automatically, a partition longer than a timeout would discard a journal that
> was about to come back and turn a short absence into a full rejoin.
> Overturned if lost hosts in practice never return, and waiting for an operator
> shows up as long under-replication.

### 5.4 Whole-cluster restart

All pods restarting together follows [RFC 10 §9.5](rfc-10-journal-replication.md#9.5%20Whole-cluster%20restart). The operator does
nothing special beyond starting the pods in parallel; each becomes ready by
§4.3.

## 6. Scaling

### 6.1 The node pool

The pool **MUST NOT** be scaled by a HorizontalPodAutoscaler. Its size is
changed only by the operator's **scaling controller**, from:

- journal fill above a threshold across most pods;
- write bandwidth near the journal devices' rate;
- under-replicated shards that repair cannot place for lack of nodes or
  failure domains.

After any change the controller **MUST** wait a cooldown of at least the shard
dwell ([RFC 11 §3.3](rfc-11-ownership.md#3.3%20The%20primary%20follows%20the%20writer)), 30 minutes as proposed, before the next.
Scale-down is never automatic unless the resource's scaling policy enables it
explicitly, and then runs the drain of §5.2.

> decision: the pool does not autoscale on connections, CPU or memory. Every
> added or removed pod moves shards, each move ships a journal tail, and a
> shard refuses a second planned move within the dwell; an autoscaler that
> follows short load swings would keep the cluster rebalancing. More
> connections are absorbed by the pods there are. Overturned if measured
> rebalance cost becomes small enough that load-following pays, or if
> protocol CPU saturates pods whose disks are idle, which is the role split's
> trigger ([RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20node%20type)).

### 6.2 Metadata store

The metadata store scales under its own operator; DittoFS needs no change when
it does. Non-normative examples: the FoundationDB operator, or the operator of
another store meeting [RFC 16](rfc-16-metadata-store.md)'s contract.

## 7. Client addressing

[RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing) requires that clients move off a lost node at
once, and that an address moves only after the lost node is fenced. Two modes
meet it; a cluster uses one.

| Mode | For | How |
| --- | --- | --- |
| `service` | clouds | a LoadBalancer Service holds a virtual address; client-IP session affinity keeps a client on one pod |
| `floatingIPs` | on-premises, layer 2 or BGP | the operator assigns each pod addresses and moves a lost pod's addresses to a survivor, as RFC 15 §5.3 describes |

Either mode **MUST**:

- **Fence before move.** No connection of a lost pod's clients is served by
  another pod before the lost pod's node lease has lapsed and every other node
  has acknowledged the fence ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing), T7). In `floatingIPs`
  mode the operator waits for the lapsed mark before moving an address. In
  `service` mode a request that reaches another pod is safe only because every
  call it forwards is refused by node epoch until then; a late forwarded write
  from the lost pod is refused the same way.
- **Reset quickly.** A client of a lost pod reconnects within a bound far below
  its transport timeout ([RFC 15 §9](rfc-15-topology.md#9.%20Conformance%20and%20benchmarks)).
- **Keep Witness and drain working.** SMB Witness names where to go, and an NFSv4
  drain answers `fs_locations` with an address that reaches another pod.

> decision: both modes are supported. A load balancer is what clouds offer and
> needs no network setup; floating addresses are what on-premises installations
> have and give the takeover RFC 15 specifies exactly. Overturned if one mode
> meets every guarantee above on both kinds of site.

## 8. Secrets and the node channel

- **Bucket credentials** are mounted on every pod, since every pod is a node
  that holds journals and offloads them.
- **Node credentials.** Each pod has a certificate bound to its node ID, issued
  by the operator or a certificate manager, and renewed before expiry. A
  decommissioned node's certificate is revoked
  ([RFC 15 §4.4](rfc-15-topology.md#4.4%20Node%20channels)).
- **The node channel** is gRPC over mutually authenticated TLS, carrying
  forwarded calls, replication and cross-shard prepares, with streaming for
  content ([RFC 15 §11](rfc-15-topology.md#11.%20Open%20questions) records the choice).
- **The store credential** is mounted on every pod.

## 9. Invariants

| # | Invariant |
| --- | --- |
| K1 | A `DittoServer` is one pod with both roles and an embedded store; a `DittoCluster` is one StatefulSet of identical pods, at least as many as the replica count. |
| K2 | A node's ID and credential are issued with its volume and never derived from its pod name; a fresh volume is a new node. |
| K3 | No pod or volume is deleted by a scale-down before its node record is decommissioned; a lost pod's volume is released only on an operator's acknowledgement or the host's return. |
| K4 | Pods are upgraded and evicted one at a time, each draining its clients and handing its shards over before it stops. |
| K5 | The pool is never scaled by a HorizontalPodAutoscaler; the scaling controller waits at least the shard dwell between changes, and scales down only under an explicit policy. |
| K6 | In either addressing mode, a lost pod's clients are served elsewhere only after its fence, and reconnect within a bound far below their transport timeout. |
| K7 | Every pod's node certificate names its node ID. |
| K8 | Pods spread by the configured failure-domain label, and each node's failure-domain setting equals that label. |

## 10. Tests

On a local cluster (kind or k3d), in the cluster tier of the
[harness](rfc-29-test-harness.md):

- **Single node:** a `DittoServer` with two replicas is refused (K1).
- **Identity:** delete a pod; assert it returns with the same node ID.
  Delete its volume claim too; assert a new node ID and that the old one is not
  reused (K2).
- **Scale-down:** lower the pool under write and client load; assert clients
  moved before the pod stops, no failover, no grace period, and the pod deleted only after its node record is
  decommissioned (K3).
- **Lost host:** cordon and kill a pod's host; assert failover within
  the bound, the volume kept, and the node reported lost (K3).
- **Upgrade:** roll the image under load; assert one pod at a time and
  every shard handed over, none failed over (K4).
- **Scaling:** drive connections up and down; assert the pool does not move.
  Fill journals; assert one pod added and no second before the cooldown (K5).
- **Address takeover:** kill a pod in each addressing mode under NFSv3,
  NFSv4 and SMB load; assert reconnects within the bound, and that a write the
  dead pod forwarded late is refused (K6).
- **Node certificates:** assert each pod's certificate names its node ID, and a
  decommissioned node's is revoked (K7).

## 11. Open questions

1. **Load balancer reset.** Whether common cloud load balancers reset a dead
   pod's connections fast enough for K6, or leave clients waiting for a TCP
   timeout.
2. **SMB Witness behind one address.** How Witness names a destination when every
   pod sits behind one virtual address in `service` mode.
3. **NFSv4 `fs_locations` in `service` mode**, where no per-pod address is
   reachable from outside.
4. **Journal volume class.** Whether network block volumes are acceptable for
   journals, given the fsync on every acknowledged write
   ([RFC 10 §4](rfc-10-journal-replication.md#4.%20The%20write%20path)).
5. **Scaling controller thresholds.** Journal fill and bandwidth thresholds are
   unmeasured.

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| Operators for distributed databases on Kubernetes | identity on the volume, operator-driven member replacement, no autoscaling of stateful members |
| Clustered storage operators (Ceph's) | one storage daemon per host and device, failure domains from node labels, removal only after data has moved |
| Bare-metal load balancers announcing addresses over layer 2 or BGP | floating addresses for on-premises sites |
