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

A DittoFS cluster has three tiers ([RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)), and each fits a
different Kubernetes shape. Protocol nodes are receptionists: they remember
nothing, so they are a Deployment that grows and shrinks freely. Storage nodes
are the workers who keep the notebooks: each has its own fast disk and a name
that must survive a restart, so they are a StatefulSet. The replicated metadata
store is the town hall, run by its own operator. The DittoFS operator is the
building manager: it adds workers slowly, and never sends one home before
handing its work to someone else. Where the picture breaks: a worker's
notebook is copied to two others, so losing one worker unannounced loses
nothing — it only costs a short pause.

![A DittoCluster resource reconciled by the operator into a protocol Deployment of at least two stateless pods scaled by an HPA, reached by clients through a Service VIP or floating IPs, and a storage StatefulSet whose pods each own a local NVMe journal volume holding their node ID and certificate, with a PodDisruptionBudget of one and one pod per host; the node channel between them is gRPC over mTLS; the metadata store runs under its own operator and is referenced, never owned; the bucket sits outside the cluster and its credential is mounted on storage pods only](img/rfc30-objects.svg)

### Scenarios

**Monday morning, connections triple.**
1. Each protocol pod reports its active client connections as a custom metric
   ([§6.1](#6.1%20Protocol%20pool)).
2. The HorizontalPodAutoscaler adds protocol pods; each serves as soon as it is
   ready, since it holds no file state ([RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)).
3. New connections spread over the new pods; existing ones stay where they are.
4. In the evening the autoscaler removes pods slowly, each draining first
   ([§5.2](#5.2%20A%20protocol%20pod%20drains%20before%20it%20stops)).

Outcome: more front-ends, no shard moved, no storage pod touched.

**A storage host dies at 10:00.**
1. The pod and its local volume are gone with the host.
2. Its node lease lapses; replicas take its shards over in about 14 s
   ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
3. The StatefulSet cannot reschedule the pod: its volume is bound to the dead
   host. The operator reports the node lost and waits
   ([§5.4](#5.4%20A%20lost%20storage%20pod)).
4. Repair restores each shard's replica count on the surviving pods
   ([RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair)).

Outcome: one failover per shard; no operator action needed to stay writable.

**The journals are filling up.**
1. The storage controller sees journal fill above its threshold on most pods
   ([§6.2](#6.2%20Storage%20pool)).
2. It raises the StatefulSet by one; the new pod joins with a fresh node ID.
3. The slot table is recomputed and about 1/N of the shards move to it,
   at a paced rate ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)).
4. The controller waits at least the cooldown before scaling again.

Outcome: one node added, a fraction of shards moved, no bucket bytes copied.

**Removing a storage pod for good.**
1. An operator lowers the storage pool size by one.
2. The operator marks the highest-ordinal pod draining; it leaves the slot table
   ([§5.3](#5.3%20Storage%20scale-down%20is%20a%20drain)).
3. Its shards are handed over one at a time, with no grace period.
4. Its node record is decommissioned; only then are the pod and its volume
   deleted.

Outcome: the cluster shrinks without a single failover.

**A node-pool upgrade.**
1. The cloud drains hosts one at a time.
2. The PodDisruptionBudget lets one storage pod go at a time
   ([§4.2](#4.2%20Storage%20pool)).
3. Each evicted storage pod hands its shards over before it stops
   ([§5.1](#5.1%20Rolling%20upgrade)).

Outcome: shards move ahead of the upgrade instead of failing over behind it.

### How the rest is organised

§1 is the purpose, §2 what the operator has today and what changes, §3 the
resources, §4 the mapping from tiers to Kubernetes objects, §5 lifecycle, §6
autoscaling, §7 client addressing, §8 secrets and the node channel, §9
invariants, §10 tests and §11 open questions.

## 1. Purpose

The core RFCs say what each tier runs and what it must guarantee
([RFC 15](rfc-15-topology.md)). This document answers:

> **Which Kubernetes objects carry each tier, how does the operator change
> their size, and what must it do before a pod goes away?**

### 1.1 Non-goals

This document **MUST NOT**:

- change any core rule: fencing, roles, placement and failover stay in RFC 10,
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
| — | `DittoCluster`: a `protocol` pool and a `storage` pool, each with size, resources and placement, and a reference to the metadata store ([§3.2](#3.2%20DittoCluster)) |
| one StatefulSet | a Deployment for protocol, a StatefulSet for storage ([§4](#4.%20Tiers%20to%20objects)) |
| pod deleted on scale-down | storage pods drained, then deleted ([§5.3](#5.3%20Storage%20scale-down%20is%20a%20drain)) |
| no autoscaling | an HPA for protocol, a storage controller for storage ([§6](#6.%20Autoscaling)) |
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
| `bucket` | the remote tier's endpoint and a credential Secret, mounted on storage pods only ([§8](#8.%20Secrets%20and%20the%20node%20channel)) |
| `protocol` | pool size bounds (`min`, `max`), resources, placement, and the autoscaling target ([§6.1](#6.1%20Protocol%20pool)) |
| `storage` | pool size, journal volume class and size, resources, placement, the failure-domain label ([§4.2](#4.2%20Storage%20pool)), and the scaling policy ([§6.2](#6.2%20Storage%20pool)) |
| `addressing` | `service` or `floatingIPs`, with their settings ([§7](#7.%20Client%20addressing)) |

Installation settings — lease, dwell, replica count and floor — are not fields
here: they live in the store as [RFC 13](rfc-13-configuration.md) settings. The resource holds only
what Kubernetes must know to place pods.

**Status** reports each pool's ready count, the cluster's health
([§4.4](#4.4%20Health)), and each share health condition the cluster reports, as a
condition on the resource.

## 4. Tiers to objects

### 4.1 Protocol pool

The protocol pool **MUST** be a Deployment with at least two replicas. Its pods
hold no file state ([RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)), mount no bucket credential, and need no
persistent volume. They spread across failure domains by a topology spread
constraint.

### 4.2 Storage pool

The storage pool **MUST** be a StatefulSet:

- **Journal volumes.** Each pod has a volume for its journals, from a class the
  resource names; local NVMe is the expected class. A pod's node identity lives
  on that volume (§4.3), so the volume is never reattached to another pod.
- **Failure domains.** Pods spread by the label the resource names — host,
  rack or zone — and the operator **MUST** set each node's failure-domain
  setting from that label, so the slot table's domain rule
  ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)) and Kubernetes placement agree. At most one storage pod
  per host.
- **Disruption budget.** A PodDisruptionBudget with `maxUnavailable: 1`, so a
  voluntary eviction never takes two members of one shard's replica set at
  once.
- **Pod management.** `podManagementPolicy: Parallel`, since pods find each
  other through the store, not through start order.

### 4.3 A storage pod's identity is its volume

A storage node's ID and its node credential ([RFC 15 §4.4](rfc-15-topology.md#4.4%20Node%20channels)) **MUST** be
issued once, when its volume is first initialised, and kept on that volume. A
pod that restarts on its volume is the same node with a new node epoch; a pod
given a fresh volume is a new node.

> decision: identity follows the volume, not the pod name. A StatefulSet
> reuses ordinals, so a name-derived identity would make a replacement pod with
> an empty journal claim to be the lost node and its acknowledged content.
> Overturned only if the journal moves off node-local volumes.

### 4.4 Health

The resource's `Ready` condition **MUST** be false while either pool has no live
pod ([RFC 15 §2.2](rfc-15-topology.md#2.2%20Startup%20refuses%20what%20cannot%20work)) or the metadata store is unreachable. Readiness probes
gate traffic: a protocol pod is ready once its adapters accept clients, a
storage pod once it has rejoined the store and holds no shard it cannot serve.

## 5. Lifecycle

### 5.1 Rolling upgrade

Storage pods **MUST** be upgraded one at a time, each handing its shards over
before it stops ([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)) and the next waiting until the upgraded pod is
ready and holds no under-replicated shard. The operator runs this itself
(`updateStrategy: OnDelete`) rather than the StatefulSet's rolling update,
which waits for readiness but not for handovers. A storage pod's `preStop`
hook **MUST** run the same handover, so an eviction the operator did not start
— a node-pool upgrade — moves shards ahead of the stop rather than failing them
over after it. Its termination grace **MUST** cover the handover bound.

> ponytail: a handover bounded by the termination grace can be cut short on a
> shard with a large un-offloaded tail, and then becomes a failover, which loses
> nothing but costs a grace period. Size the grace from the handover benchmark
> ([RFC 15 §9](rfc-15-topology.md#9.%20Conformance%20and%20benchmarks)) when real tails make cut-short handovers show up.

### 5.2 A protocol pod drains before it stops

A protocol pod's `preStop` hook **MUST** drain before the pod exits: tell SMB
clients through Witness and NFSv4 clients through `fs_locations` where to go
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)), stop accepting new connections, and wait up to the
termination grace for connections to leave. NFSv3 clients and those left at the
deadline reconnect through the address ([§7](#7.%20Client%20addressing)). An NFSv4.1 session on the pod
ends with it; its client creates a new one ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)).

### 5.3 Storage scale-down is a drain

Lowering the storage pool **MUST** run, for the pod being removed:

1. mark the node draining, which takes it out of the placement set and
   recomputes the slot table ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards));
2. hand each shard it is primary of over, and replace it as replica, under the
   ordinary rules and pace ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries));
3. once it is primary and replica of nothing, decommission its node record;
4. only then delete the pod and its volume.

The operator **MUST NOT** delete a storage pod or its volume before step 3. A
scale-down that would leave fewer pods than the replica count, or fewer failure
domains than the slot table needs, **MUST** be refused.

### 5.4 A lost storage pod

A storage pod whose host is lost is a failover, not a scale-down: its shards
fail over ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) and repair restores their counts
([RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair)). The pod cannot be rescheduled while its node-local volume is
bound to the lost host. The operator **MUST NOT** release that volume on its own:
the host may return within the re-placement delay, with its journal. It reports
the node lost, and an operator's acknowledgement — or the host's return —
decides. After acknowledgement the operator decommissions the node record,
deletes the volume claim, and the StatefulSet creates a pod with a new volume,
which is a new node (§4.3).

> decision: releasing a lost pod's volume waits for an operator. Released
> automatically, a partition longer than a timeout would discard a journal that
> was about to come back and turn a short absence into a full rejoin.
> Overturned if lost hosts in practice never return, and waiting for an operator
> shows up as long under-replication.

![Side by side: a planned storage scale-down marks the pod draining, hands over its shards, decommissions its node record and only then deletes the pod and volume, with no failover, and is refused if it would leave fewer pods than the replica count or too few failure domains; a lost host fails over in about 14 s, repair restores replica counts, the volume is kept and the node reported lost until either the host returns as the same node with its journal or an operator acknowledges, after which the node record is decommissioned, the claim deleted and a new pod with a new volume becomes a new node](img/rfc30-drain.svg)

### 5.5 Whole-cluster restart

All pods restarting together follows [RFC 10 §9.5](rfc-10-journal-replication.md#9.5%20Whole-cluster%20restart). The operator does
nothing special beyond starting the storage pool's pods in parallel; protocol
pods become ready only once storage pods serve.

## 6. Autoscaling

### 6.1 Protocol pool

The protocol pool **SHOULD** be scaled by a HorizontalPodAutoscaler on active
client connections per pod, a custom metric each pod exports, with CPU as a
second target. Scale-down **MUST** be slow — a stabilization window of at least
10 minutes — because each removal drains clients (§5.2).

### 6.2 Storage pool

The storage pool **MUST NOT** be scaled by a HorizontalPodAutoscaler. Its size is
changed only by the operator's **storage controller**, from:

- journal fill above a threshold across most pods;
- write bandwidth near the journal devices' rate;
- under-replicated shards that repair cannot place for lack of nodes or
  failure domains.

After any change the controller **MUST** wait a cooldown of at least the shard
dwell ([RFC 11 §3.3](rfc-11-ownership.md#3.3%20The%20primary%20follows%20the%20writer)), 30 minutes as proposed, before the next.
Scale-down is never automatic unless the resource's scaling policy enables it
explicitly, and then runs the drain of §5.3.

> decision: storage does not autoscale on CPU or memory. Every added or removed
> storage pod moves shards, each move ships a journal tail, and a shard refuses
> a second planned move within the dwell; an autoscaler that follows short load
> swings would keep the cluster rebalancing. Overturned if measured rebalance
> cost becomes small enough that load-following pays.

![Two columns: the protocol pool is scaled by a HorizontalPodAutoscaler on active connections per pod and CPU, new pods serve once ready, existing connections stay put, and scale-down waits a stabilization window of at least 10 minutes because each removal drains; the storage pool is scaled only by the operator's storage controller from journal fill, write bandwidth and unplaceable under-replicated shards, adds one pod with a fresh node ID, moves about 1/N of shards at a paced rate, waits a cooldown of at least the shard dwell, 30 minutes as proposed, and never scales on CPU or memory or down without an explicit policy](img/rfc30-autoscaling.svg)

### 6.3 Metadata store

The metadata store scales under its own operator; DittoFS needs no change when
it does. Non-normative examples: the FoundationDB operator, or the operator of
another store meeting [RFC 16](rfc-16-metadata-store.md)'s contract.

## 7. Client addressing

[RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing) requires that clients move off a lost protocol node at
once, and that an address moves only after the lost node is fenced. Two modes
meet it; a cluster uses one.

| Mode | For | How |
| --- | --- | --- |
| `service` | clouds | a LoadBalancer Service holds a virtual address; client-IP session affinity keeps a client on one pod |
| `floatingIPs` | on-premises, layer 2 or BGP | the operator assigns each protocol pod addresses and moves a lost pod's addresses to a survivor, as RFC 15 §5.3 describes |

Either mode **MUST**:

- **Fence before move.** No connection of a lost pod's clients is served by
  another pod before the lost pod's node lease has lapsed and every storage node
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

- **Bucket credentials** are mounted on storage pods only. A protocol pod
  **MUST NOT** mount them ([RFC 15 §2.3](rfc-15-topology.md#2.3%20What%20a%20role%20does%20not%20hold)).
- **Node credentials.** Each pod has a certificate bound to its node ID, issued
  by the operator or a certificate manager, and renewed before expiry. A
  decommissioned node's certificate is revoked
  ([RFC 15 §4.4](rfc-15-topology.md#4.4%20Node%20channels)).
- **The node channel** is gRPC over mutually authenticated TLS, carrying
  forwarded calls, replication and cross-shard prepares, with streaming for
  content ([RFC 15 §11](rfc-15-topology.md#11.%20Open%20questions) records the choice).
- **The store credential** is mounted on every pod; protocol pods use it within
  the writes [RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut) allows them.

## 9. Invariants

| # | Invariant |
| --- | --- |
| K1 | A `DittoServer` is one pod with both roles and an embedded store; a `DittoCluster` has a protocol Deployment of at least two pods and a storage StatefulSet. |
| K2 | A storage node's ID and credential are issued with its volume and never derived from its pod name; a fresh volume is a new node. |
| K3 | No storage pod or volume is deleted by a scale-down before its node record is decommissioned; a lost pod's volume is released only on an operator's acknowledgement or the host's return. |
| K4 | Storage pods are upgraded and evicted one at a time, each handing its shards over before it stops. |
| K5 | The storage pool is never scaled by a HorizontalPodAutoscaler; the storage controller waits at least the shard dwell between changes, and scales down only under an explicit policy. |
| K6 | In either addressing mode, a lost protocol pod's clients are served elsewhere only after its fence, and reconnect within a bound far below their transport timeout. |
| K7 | Bucket credentials are mounted on storage pods only; every pod's node certificate names its node ID. |
| K8 | Storage pods spread by the configured failure-domain label, and each node's failure-domain setting equals that label. |

## 10. Tests

On a local cluster (kind or k3d), in the cluster tier of the
[harness](rfc-29-test-harness.md):

- **Single node:** a `DittoServer` with two replicas is refused (K1).
- **Identity:** delete a storage pod; assert it returns with the same node ID.
  Delete its volume claim too; assert a new node ID and that the old one is not
  reused (K2).
- **Scale-down:** lower the storage pool under write load; assert no failover,
  no grace period, and the pod deleted only after its node record is
  decommissioned (K3).
- **Lost host:** cordon and kill a storage pod's host; assert failover within
  the bound, the volume kept, and the node reported lost (K3).
- **Upgrade:** roll the storage image under load; assert one pod at a time and
  every shard handed over, none failed over (K4).
- **Autoscaling:** drive connections up and down; assert the protocol pool
  follows and the storage pool does not move. Fill journals; assert one storage
  pod added and no second before the cooldown (K5).
- **Address takeover:** kill a protocol pod in each addressing mode under NFSv3,
  NFSv4 and SMB load; assert reconnects within the bound, and that a write the
  dead pod forwarded late is refused (K6).
- **Secrets:** assert no protocol pod mounts the bucket Secret (K7).

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
5. **Storage controller thresholds.** Journal fill and bandwidth thresholds are
   unmeasured.

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| Operators for distributed databases on Kubernetes | identity on the volume, operator-driven member replacement, no autoscaling of stateful members |
| Clustered storage operators (Ceph's) | one storage daemon per host and device, failure domains from node labels, removal only after data has moved |
| Bare-metal load balancers announcing addresses over layer 2 or BGP | floating addresses for on-premises sites |
