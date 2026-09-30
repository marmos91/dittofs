---
rfc: 13
title: "RFC 13 — configuration"
component: configuration
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-12-snapshots]]"
aliases:
  - RFC 13
tags:
  - rfc
---
# RFC 13 — configuration

**Status:** draft. [§11](#11.%20Open%20questions) has no open questions left; [§10](#10.%20Edits%20this%20document%20asks%20of%20other%20RFCs)
lists the edits it asks of RFCs already reviewed.
**Audience:** anyone adding a setting, implementing the control plane's records,
or deciding what an operator may change and when. Conventions and test tiers are
in [the RFC index](rfc-index.md).

This document specifies behaviour, not the current code.
[Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists where the code differs.

## In short

- Configuration lives in the **control plane**. A host holds only what it needs
  to reach it: its identity, where the metadata store is, and its local
  devices.
- Most quantities are **fixed**, not settings. A value becomes a setting only
  when an operator can name a workload the fixed value is wrong for.
- Every setting has one **scope** — installation, node, namespace, remote store
  or share — and one **binding class**, which says what changing it does: apply
  now, apply at restart, govern the next write, or nothing, because it is bound
  to content already stored.
- A setting bound to stored content **MUST NOT** change while content exists.
  Changing it is a migration ([RFC 12](rfc-12-snapshots.md)), never an edit.
- An invalid setting is refused, twice: by the control plane when it is written,
  and by the component when it is built. It is never replaced by a default.
- Configuration never holds a secret. It holds a reference to one.

## 1. Purpose

The storage RFCs each name the few settings their component takes, and each
states some rule about them: refused rather than replaced ([RFC 8 §2.4](rfc-8-engine.md#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced)), two
settings and everything else fixed ([RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)), configuration governs the
next write ([RFC 5 §5.1](rfc-5-transforms.md#5.1%20Configuration%20governs%20the%20next%20write)). This document collects those rules into one model,
gives every setting a scope and a binding class, and states what the control
plane owes the components that read it.

### 1.1 Non-goals

This document **MUST NOT** be read as specifying:

- the management API, its authentication or its resources: [RFC 23](rfc-index.md);
- what each setting means inside its component: that stays in the component's
  RFC, which this document cites;
- how the metadata store replicates: it is a store that runs consensus
  ([RFC 16](rfc-16-metadata-store.md)); this document states only what it must hold and answer.

## 2. Where configuration lives

### 2.1 The control plane is the source

Every setting is a field of a record in the **metadata store**, held by the
control plane. Components receive records; they do not read files, and no
component has a configuration source of its own. Two sources for one setting are
two answers to one question, and the one that wins is whichever the code
happened to read last.

Settings live in the metadata store's KV ([RFC 16](rfc-16-metadata-store.md)): each setting is a
`Setting` record at its scope, beside users, shares and the file metadata, so
one database is run, backed up and replicated for all of it. The store's format
record, read before anything else at open, covers these records as it covers
every other ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)).

### 2.2 A host holds only its bootstrap

A process needs a few facts before it can reach the control plane. These, and
only these, come from the host — a file or the environment:

| Bootstrap fact                                                                                                        | Why it cannot come from the control plane                                                                                                 |
| --------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| the node's identity                                                                                                   | the control plane addresses the node by it                                                                                                |
| how to reach the metadata store, and the credential for it                                                            | needed to read anything else                                                                                                              |
| the local devices the node may use, and their paths                                                                   | a path means something only on its host ([§3](#3.%20Scopes))                                                                              |
| listen addresses                                                                                                      | a port conflict is a host's problem                                                                                                       |
| log destination and level                                                                                             | needed before the control plane answers, to say why it did not                                                                            |
| the node's **roles** — `protocol`, `storage`, both by default ([RFC 15](rfc-15-topology.md))                          | they decide what the node composes, including whether it reaches the control plane's store as a writer; fixed for the life of the process |
| the location of the wrapping key of each role the node runs, and no other, for `Secret` records ([§7](#7.%20Secrets)) | a key kept in the store it protects protects nothing, and a key for a role the node does not run is a key it can leak                     |

A bootstrap fact **MUST NOT** also be a record field, and a record field
**MUST NOT** be overridable from the host. An override from the host is a
second source ([§2.1](#2.1%20The%20control%20plane%20is%20the%20source)): nodes of one installation then disagree, silently.
A provisioning file ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)) is not an override: it writes records through the
control plane, and the record then says it came from the file.

### 2.3 A record is versioned

Each record carries a **generation**, raised by every change, and a **schema
version**. A component reports, per record, the generation it runs
([§8](#8.%20Observability)). A reader that meets a schema version it does not know refuses
the record rather than reading the fields it recognises: a field it skipped may
be the one that mattered.

### 2.4 Records can be declared in a provisioning file

An operator **MAY** declare records in a **provisioning file**, so a server or a
test starts in a known state without a script of API calls, and the file can be
kept under version control. Its path is a bootstrap fact. The file declares
records by their API names — settings at any scope, remote stores, namespaces,
shares, users, groups, grants, snapshot policies — in the same shape the
management API accepts.

**Applying is a merge through the API.** At start, and on every reload, the node
validates the whole file with the API's validator ([§6](#6.%20Validation)) and then writes
each declared record as the API would: created if absent, replaced if it
differs. Records the file does not declare are left alone. A file that fails
validation, or whose change the API would refuse — a bound setting with content
behind it ([§5.1](#5.1%20A%20bound%20setting%20refuses%20change)), for instance — is refused whole: at start the node refuses to
start, and on reload the running records stay as they were and the node reports
a health condition naming the record and the reason. Nothing is half-applied.

**A declared record has one source, the file.** Each record the file writes is
marked **managed**, with the file's path and content hash. The API **MUST**
refuse to change or delete a managed record, answering that it is declared in
that file and must be changed there and reloaded. This keeps §2.1's rule: every
record has exactly one source, and an operator can see which.

| Event | Effect |
| --- | --- |
| a declared record differs from the store | the file's value is written |
| a record is removed from the file | it stays, and stops being managed; with `prune: true` in the file it is deleted instead, subject to the same refusals as an API delete |
| an API call edits a managed record | refused, naming the file |
| two nodes apply files that declare the same record differently | the second is refused: a record managed by another file hash is changed only after the first file stops declaring it; health condition on the refused node |
| reload | `dfsctl config reload`, a signal, or at start; the node reports the applied file hash ([§8](#8.%20Observability)) |

**Secrets stay references.** A provisioning file **MUST NOT** hold a secret value.
It names one by reference — an environment variable or a file on the host — and
applying seals the value into a `Secret` record ([§7](#7.%20Secrets)) as the API would.

**Node-scoped records** a file declares apply only to the node that reads it.
Every other scope is installation-wide, so in a cluster one node, or an
identical file on every node, provisions it.

> ponytail: a node applies its file only at start and on an explicit reload; it
> does not watch the file. Add a watch when operators edit files in place often
> enough that a forgotten reload shows up as drift.

## 3. Scopes

A setting has exactly one scope, the smallest thing it must be the same across:

| Scope | Holds | Examples |
| --- | --- | --- |
| **installation** | what every node and namespace shares | the syncer's pool sizes' defaults, GC's schedule |
| **node** | what describes one host's resources | its journals, one per device, and each one's maximum footprint ([RFC 1 §7](rfc-1-journal.md#7.%20Capacity)) |
| **namespace** | what decides stored bytes and their identity | key scope, chunking key, `Target`, the counting domain |
| **remote store** | how blocks reach and sit on a service | endpoint, bucket, prefix, credential reference, storage class, transform chain, block target |
| **share** | what a client sees | case sensitivity, name length, `atime`, per-share journal limit, snapshot policy |

**A setting that decides stored bytes belongs to the scope content is compared
across.** Deduplication compares chunks across a namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)), so
whatever decides a chunk's boundaries or its identity is a namespace setting. A
share-level `Target` would let two shares of one namespace cut the same file
differently and stop deduplicating each other without saying so.

**A namespace has one remote store**, and a store serves one namespace
([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)). The transform chain is a store setting ([RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration)), and so,
through this pairing, the same for every share of the namespace.

## 4. Settings and fixed values

### 4.1 Fixed by default

A quantity is **fixed** unless an operator can name a workload the fixed value
is wrong for, generalising [RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed). A setting is a promise that every
value in its range works, a range to test, and one more way to misconfigure a
deployment. A fixed value that turns out wrong is changed in a release.

Between the two sit **derived** values, computed from settings or from the
host (the carver's bounds from `Target`, the connection pool from its callers),
and **measured** values, set once by a tool ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)). Neither is
configured.

### 4.2 Every setting has a default, or is required

A setting either has a default stated in its RFC or is **required**: a record
that omits it is refused. There is no third kind — a setting with no default
that the code fills in is a default nobody reviewed.

Required, because no default is safe:

- a remote store's endpoint, bucket and credential reference;
- a namespace's key scope ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope));
- a node's journal devices;
- the material provider, when the chain encrypts ([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)).

Everything else takes a default. [Appendix B](#Appendix%20B%20%E2%80%94%20the%20settings) lists the settings the RFCs
name, with the default each states or, where none does, the one proposed here.

## 5. Binding classes

What changing a setting does is a property of the setting, declared with it:

| Class | A change takes effect | Examples |
| --- | --- | --- |
| **live** | on the next operation that reads it, on every node | snapshot schedule, GC interval, `atime` policy |
| **restart** | when the process next starts | pool sizes ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)), listen addresses |
| **next write** | for content written from then on; stored content keeps what it was written with and stays readable | transform chain and its settings ([RFC 5 §5.1](rfc-5-transforms.md#5.1%20Configuration%20governs%20the%20next%20write)), storage class, block target |
| **bound** | never, once content exists | key scope, chunking key, `Target`, bucket, prefix, case sensitivity |

**A live change reaches every node within 5 s.** Nodes watch the settings
records in the metadata store rather than poll them, and a node **MUST** run a
live change within 5 s of its commit. While a change spreads, nodes **MAY** run
different generations of one record; each reports the generation it runs
([§8](#8.%20Observability)). A setting whose nodes **MUST** agree at every instant — a node
lease, the drift bound — is therefore not live: it belongs to the **restart**
class, where the change is taken up by a planned restart of every node.

### 5.1 A bound setting refuses change

A **bound** setting decides where stored content is or what its identity is, so
changing it in place does not change the content — it makes the content
unreachable, or makes new content fail to match old. Repointing a store's prefix
loses every block, because a block's location is derived from its name and the
store's configuration ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)); changing `Target` re-cuts every file and
deduplicates nothing against what is stored ([RFC 2 §3.6](rfc-2-carver.md#3.6%20Changing%20any%20of%20this%20is%20a%20migration)).

So the control plane **MUST** refuse a change to a bound setting while the
namespace or store holds content, and a share's binding to its namespace
**MUST NOT** change while the share holds content, except by a re-home
([RFC 12 §4.7](rfc-12-snapshots.md#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), the one sanctioned change of that binding: it copies the
share's content into a new namespace while the share serves, and rebinds it only
once no ref names the old one. What an operator wants from such a change is a
migration — a re-home into a new namespace, or a move between installations
([RFC 12 §4](rfc-12-snapshots.md#4.%20Moving%20a%20namespace%20between%20installations)) — which leaves the old content where it can still be read
until the move completes.

### 5.2 Reaching the same content another way is not a change

Some fields name how to reach content, not where it is: a store's endpoint, and
its credential. A new endpoint for the same bucket, or a rotated credential, is
allowed, and is proven rather than trusted: the store's capability check
([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)) **MUST** find, at the new location, the namespace claim the old
one held ([RFC 12 §4.1](rfc-12-snapshots.md#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)). A location that answers but holds another claim, or
none, is a different store, and the change is refused.

### 5.3 What content was written with is recorded with it

A **next write** setting is recorded with the content it governed, where a later
reader or a census can find it: the chain ID in every block name, the census in
every block record ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)). A **bound** setting is recorded once, with
the namespace, when it is created, and compared with configuration at every
open: a namespace whose recorded `Target` or key scope differs from its record
does not open ([RFC 8 §2.4](rfc-8-engine.md#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced)). Reading never consults configuration for how
content was written ([RFC 5 §2.5](rfc-5-transforms.md#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)).

### 5.4 A format version is written only once every reader reads it

The block format version a namespace writes ([RFC 4 §3.5](rfc-4-remote-tier.md#3.5%20Format%20changes%20are%20migrations)) is a **namespace
setting**, next write, defaulting to the newest version every node of the
installation reads. The control plane **MUST** refuse to advance it past the
oldest version any node that may read the namespace supports, and a node
**MUST** refuse to join while a namespace it would serve writes a version it
cannot read. A newer binary alone therefore never writes blocks an older one,
still serving, would refuse.

## 6. Validation

**Refused, never replaced** ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)). A value out of range, of the wrong
type, or naming something that does not exist **MUST** be refused with an error
naming the field and the rule. A component **MUST NOT** clamp it, round it, or
fall back to a default: the operator asked for something, and running something
else is a lie the operator finds out about from a failure.

**Checked twice.** The control plane validates a record when it is written,
so an operator sees the error at the moment of the mistake. The component
validates again when it is built ([RFC 8 §2.4](rfc-8-engine.md#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced)), because a record can predate the
rule, or come from a newer control plane. Both run the same rules, owned by the
component: the control plane calls the component's validator, and has none of
its own to drift.

**Unknown fields are refused.** A field the schema does not know is most often a
misspelling of one it does, and ignoring it silently runs the default.

**A record that does not validate stops only what it configures.** A share whose
record is invalid does not start; the rest of the installation does.

## 7. Secrets

Configuration **MUST NOT** hold a secret value: not a credential, not a key, not
a passphrase. It holds a **reference** — a secret's name in a secret provider —
and the component resolves it when it is built.

The default secret provider is the metadata store itself: a `Secret`
record ([RFC 16](rfc-16-metadata-store.md)) in the same KV, its value **envelope-encrypted** under a
wrapping key held outside that KV — a host file or an external key service,
named by the bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)). A copy of the store alone reveals no
secret.

**Wrapping keys are per role.** Each secret kind is sealed under the wrapping
key of the role that uses it, and a node's bootstrap names only the keys of its
own roles:

| Wrapping key | Seals | Needed by |
| --- | --- | --- |
| `protocol` | password hashes, NT hashes, keytabs, identity-provider bind credentials | the `protocol` role: authentication |
| `storage` | remote-tier credentials, references to master keys | the `storage` role: the remote tier and the material provider |

So a compromised protocol-only node, which must read the KV, holds sealed
remote-tier credentials it cannot open. A deployment **MAY** split a role's key
further by secret kind; it **MUST NOT** merge the two roles' keys unless every
node runs both roles, which is the single-node default.

**One interface, two providers.** Every secret is resolved through one
interface. The built-in provider above — sealed `Secret` records under the role
wrapping keys — ships now; an external key-management provider comes later,
behind the same interface and the same references, so adding it changes no
record. Signatures are indicative:

```go
// SecretProvider resolves a secret reference to its value. It never lists
// values and never returns one through any other path.
type SecretProvider interface {
	// Resolve returns the current value of ref, unsealed with the wrapping key
	// of role. It fails if this node does not hold that role's key.
	Resolve(ctx context.Context, role Role, ref SecretRef) ([]byte, error)
	// Seal stores value under ref, sealed with role's wrapping key, and
	// returns the reference to record in configuration.
	Seal(ctx context.Context, role Role, ref SecretRef, value []byte) (SecretRef, error)
	// Delete destroys the secret; a later Resolve of ref fails.
	Delete(ctx context.Context, ref SecretRef) error
}
```

- **Read never returns a secret.** No API, export, log line or metric carries
  one. An export that must travel with its secrets, a backup that must be
  restorable ([RFC 12 §4.4](rfc-12-snapshots.md#4.4%20Key%20scope%20and%20material)), carries references, and says which secrets the
  destination must hold.
- **A secret changes without a record change.** Rotating a credential updates the
  provider; the reference stays, and the component picks the new value up on its
  next resolution ([§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change)).
- **Material is a secret with a lifecycle.** Encryption keys are resolved through
  the material provider, which also tracks which keys exist, are current or are
  destroyed ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)). A secret provider need not.
- **Material configuration is master keys plus wrapped namespace keys.** A remote
  store's configuration references its **master keys**, which the provider holds
  and never releases. Each namespace's keys — its current and retired **data
  keys**, its **header key** and its **chunking key** — are stored only wrapped
  under a master key, and the namespace's record holds each one's (material ID,
  fingerprint), never the key. The provider refuses an ID whose fingerprint
  changed. Rotating a master key re-wraps the namespace keys and changes no
  record; rotating a data key is new material plus relocation
  ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)).
- **A namespace's header key and chunking key** are derived at its creation when
  its chain encrypts ([RFC 2 §6](rfc-2-carver.md#6.%20Boundaries%20are%20public)) and live as long as the namespace. An
  export names them, and every data key in its census, the same way
  ([RFC 12 §4.4](rfc-12-snapshots.md#4.4%20Key%20scope%20and%20material)).

- The only secrets on a host are the bootstrap credential for the configuration
  store and the wrapping keys of its own roles, or references to them in an
  external key service ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)).

## 8. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| the generation of each record a node runs, against the store's current one | `dittofs_config_generation` | gauge, labelled by record |
| records refused, by component and reason | `dittofs_config_refused_total` | counter |
| changes refused because a setting is bound | `dittofs_config_bound_refusals_total` | counter |
| the provisioning file hash each node applied, and applies refused by reason | `dittofs_config_provisioning_applied`, `dittofs_config_provisioning_refused_total` | gauge, counter |

A node whose generation for a live record stays behind the store's for longer
than the 5 s propagation bound ([§5](#5.%20Binding%20classes)) is a health condition of that node, not
only a gauge.

## 9. Test plan

Conventions and tiers are the index's ([Test tiers](rfc-index.md#Test%20tiers)).

| Requirement | Check |
| --- | --- |
| [§5.1](#5.1%20A%20bound%20setting%20refuses%20change) bound | For every bound setting, write content, then change the setting through the API. Assert refused, and content still readable. Repeat with no content; assert accepted. |
| [§5.1](#5.1%20A%20bound%20setting%20refuses%20change) share binding | Rebind a share with content to another namespace. Assert refused. |
| [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) same content | Change a store's endpoint to a second address of the same bucket; assert accepted. Change it to another bucket; assert refused, and the store still serving the old one. |
| [§5.3](#5.3%20What%20content%20was%20written%20with%20is%20recorded%20with%20it) recorded | Edit a namespace's stored `Target` underneath the record. Assert the namespace does not open. |
| [§6](#6.%20Validation) refused | For every setting, feed one value out of range and one of the wrong type, through the API and directly to the component. Assert both refuse, with the field named; assert no component runs a default in its place. |
| [§6](#6.%20Validation) one validator | Assert the API and the component return the same error for every invalid record in the fixture set. |
| [§6](#6.%20Validation) unknown field | Misspell one field of each record. Assert refused. |
| [§7](#7.%20Secrets) role keys | Start a protocol-only node. Assert its bootstrap names no `storage` wrapping key and that unsealing a remote-tier credential from it fails. |
| [§7](#7.%20Secrets) no secret out | Configure every secret-bearing record; read every API, export and log produced by a full test run. Assert no secret value appears. |
| [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap) no host override | Set a record field in the host's environment. Assert it is refused at start, not applied. |
| [§2.3](#2.3%20A%20record%20is%20versioned) schema | Present a record with a newer schema version. Assert refused. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) provisioning | Start a node with a file declaring a namespace, a share and a setting. Assert all three exist and are marked managed. Edit the share through the API; assert refused, naming the file. Change the file and reload; assert applied. Remove the share from the file; assert it stays, unmanaged; add `prune: true`; assert deleted. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) all or nothing | Reload a file with one valid change and one invalid record, then one changing a bound setting with content behind it. Assert nothing changed, the health condition names the record, and at start the node refuses to start. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) secrets | Declare a remote store whose credential is an environment variable. Assert the file holds no value, the record references a sealed `Secret`, and no API or log shows the value. |
| [§5.4](#5.4%20A%20format%20version%20is%20written%20only%20once%20every%20reader%20reads%20it) format version | Run two nodes, one reading one format version fewer. Advance the namespace's version to write; assert refused. Upgrade the second node; assert accepted, and every block either node writes afterwards reads on both. |
| [§7](#7.%20Secrets) namespace keys | Replace a namespace's chunking key under the same ID. Assert the provider refuses it by fingerprint and the namespace does not open. |

## 10. Edits this document asks of other RFCs

Proposed, for review with this document; none is applied yet.

1. **RFC 2 §3.2:** `Target` becomes a namespace setting, not a share's
   ([§3](#3.%20Scopes)), and bound ([§5](#5.%20Binding%20classes)).
2. **RFC 2 §6 and RFC 5 §3.2:** "a share that encrypts" reads "a namespace whose
   store's chain encrypts"; the chunking key is the namespace's.
3. **RFC 4 §4.2 and Appendix C:** bucket and prefix are bound; endpoint and
   credential change under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change).
4. **RFC 12 §6.2:** the YAML example becomes the control-plane records it
   describes: `snapshots.hold_bound` and `snapshots.reserve` per share, `snapshots.hold_journal_fraction`
   and `migration.freeze_timeout` per installation (Appendix B).
5. **RFC 7 §3.3:** case sensitivity is bound, so a change is refused rather than
   left undefined.
6. **RFC 9 §8:** GC's `Config` holds four namespace fields (interval, trash
   retention, space-amplification target, audit period); everything else it
   once held is a constant or derived.

## 11. Open questions

None.

---

## Appendix A — where the current code differs

Descriptive, for the refactor.

| # | This document says | The code today |
| --- | --- | --- |
| C1 | One source, the control plane ([§2.1](#2.1%20The%20control%20plane%20is%20the%20source)) | two layers, a host file with environment overrides and a control-plane database, merged key by key; the chunking settings come only from the host file and apply to every share on the node |
| C2 | Bound settings refuse change ([§5.1](#5.1%20A%20bound%20setting%20refuses%20change)) | a share's metadata store and block store can be rebound with no check for existing content; a store's type, bucket, endpoint and prefix can be edited, and a metadata store's type and settings are updated without validation |
| C3 | Refused, never replaced ([§6](#6.%20Validation)) | several settings fall back silently: an invalid chunk profile keeps the default, an out-of-range upload parallelism turns adaptive, an empty compression algorithm becomes zstd, a non-boolean durability flag is ignored, a zero backpressure wait becomes 60 s (its own range check can never fire), a zero Kerberos uid or gid becomes 65534 |
| C4 | Unknown fields refused ([§6](#6.%20Validation)) | unknown host-file keys are warnings; store settings are an untyped document |
| C5 | No secret in configuration ([§7](#7.%20Secrets)) | remote store credentials, the LDAP bind password and Kerberos settings are stored in the configuration database in plain text; reads redact them |
| C6 | Node-local facts only on the host ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)) | the machine SID and the token-signing secret are per-host and must be copied between nodes by hand; key-file paths held in the shared database must exist on every node |
| C7 | Every setting read by something | a generic settings table and its CLI have no reader; the SMB encryption adapter setting is a stub |
| C8 | One default per setting ([§4.2](#4.2%20Every%20setting%20has%20a%20default%2C%20or%20is%20required)) | log retention defaults differ between the file path and the built-in defaults; a documented journal log limit is described as gating writes and does not |
| C9 | Host fields not overridable, flags or otherwise | the documented order names command-line flags as the highest source; none binds to a setting |

## Appendix B — the settings

Every setting the storage RFCs name, with its scope and class. "Proposed" marks a
default this document suggests where the owning RFC states none.

| Setting | Defined in | Scope | Class | Default |
| --- | --- | --- | --- | --- |
| journal devices and paths | [RFC 1](rfc-1-journal.md) | node (bootstrap) | restart | required |
| journal maximum footprint | [RFC 1 §7](rfc-1-journal.md#7.%20Capacity) | node, per journal | live | proposed: 80% of the device |
| per-share journal limit | [RFC 1 §7](rfc-1-journal.md#7.%20Capacity) | share | live | proposed: the journal's maximum |
| segment size | [RFC 1 §4.2](rfc-1-journal.md#4.2%20Segments) | fixed | — | open in RFC 1 |
| headroom for records without bytes (count of removal and durable records reserved) | [RFC 1 §7](rfc-1-journal.md#7.%20Capacity) | node, per journal | restart | a proposal ([RFC 1 open question 7](rfc-1-journal.md#12.%20Open%20questions)) |
| idle-seal threshold | [RFC 1 §9.1](rfc-1-journal.md#9.1%20Rebuilding) | node, per journal | restart | proposed: 16 MiB ([RFC 1 open question 7](rfc-1-journal.md#12.%20Open%20questions)) |
| `Target` | [RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it) | namespace | bound | 256 KiB |
| key scope | [RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope) | namespace | bound | required |
| chunking key | [RFC 2 §6](rfc-2-carver.md#6.%20Boundaries%20are%20public) | namespace | bound | derived at creation when the chain encrypts; a secret, by reference ([§7](#7.%20Secrets)) |
| header key | [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) | namespace | bound | derived at creation when the chain encrypts; wrapped, by (ID, fingerprint) ([§7](#7.%20Secrets)) |
| data key, current | [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) | namespace | next write | created with the namespace when the chain encrypts; wrapped, by (ID, fingerprint) ([§7](#7.%20Secrets)) |
| master keys | [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) | remote store | live | required when the chain encrypts; references only ([§7](#7.%20Secrets)) |
| block format version to write | [§5.4](#5.4%20A%20format%20version%20is%20written%20only%20once%20every%20reader%20reads%20it) | namespace | next write | the newest version every node reads |
| `upload_workers`, `fetch_workers` | [RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed) | installation | restart | 128 each, or the sizing tool's |
| endpoint, credential reference | [RFC 4 §4.1](rfc-4-remote-tier.md#4.1%20Interface) | remote store | live, under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) | required |
| bucket, prefix | [RFC 4 Appendix C](rfc-4-remote-tier.md#Appendix%20C%20%E2%80%94%20the%20S3-compatible%20block%20store) | remote store | bound | required |
| storage class | [RFC 4 §8](rfc-4-remote-tier.md#8.%20Decisions%20and%20open%20questions) | remote store | next write | the service's default |
| block target | [RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler) | remote store | next write | 4 MiB; confirm by the block-size benchmark against each service before release ([RFC 4 Appendix B](rfc-4-remote-tier.md#Appendix%20B%20%E2%80%94%20measurements)) |
| transform chain, `require` | [RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration) | remote store | next write | empty |
| material provider | [RFC 5 §2.5](rfc-5-transforms.md#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material) | remote store | live | required when the chain encrypts |
| case sensitivity: the share's fold rule, by ID from the store format record ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)) | [RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case) | share | bound | sensitive (identity rule) |
| `atime` policy | [RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps) | share | live | proposed: relative |
| `gc.interval`: between compaction and collection passes | [RFC 9 §8](rfc-9-gc.md#8.%20API%20surface) | namespace | live | open in RFC 9 |
| `gc.trash_retention`: time a retired block with recoverable chunks waits before its delete | [RFC 9 §3.7](rfc-9-gc.md#3.7%20Trash) | namespace | live | 48 h |
| `gc.space_amp_target`: stored over referenced bytes the compactor holds the namespace under; 0 turns compaction off | [RFC 9 §4.4](rfc-9-gc.md#4.4%20When%20to%20compact%20is%20policy) | namespace | live | proposed: 1.25 |
| `gc.audit.period`: time within which the audit covers every chunk and block record; its rate is derived from it | [RFC 9 §6.1](rfc-9-gc.md#6.1%20Coverage) | namespace | live | 7 days |
| replica count | [RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement) | installation default; per shard, in its shard record | live | 3 |
| replica floor | [RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement) | installation default; per shard, in its shard record | live | 2 |
| failure domain | [RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement) | installation | live | open in RFC 10 |
| node lease duration | [RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) | installation | restart ([§5](#5.%20Binding%20classes)) | 10 s; confirm by the takeover-time benchmark ([RFC 10 §15](rfc-10-journal-replication.md#15.%20Test%20plan%20and%20benchmarks)) |
| node lease renewal interval | [RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) | installation | restart | 3 s; confirm by the takeover-time benchmark ([RFC 10 §15](rfc-10-journal-replication.md#15.%20Test%20plan%20and%20benchmarks)) |
| drift bound | [RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) | installation | restart | 500 ms; confirm by the takeover-time benchmark ([RFC 10 §15](rfc-10-journal-replication.md#15.%20Test%20plan%20and%20benchmarks)) |
| open-state lease: NFSv4 lease period, SMB durable-handle timeout | [RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease) | fixed | — | NFSv4 90 s; SMB per the protocol |
| default request deadline, for an operation that arrives with none | [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values) | fixed | — | 30 s |
| per-child shard slot count | [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | installation, fixed at its creation | bound | 4096 |
| capacity weight | [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | node | live | proposed: 1, every node equal |
| shard | [RFC 11 §2](rfc-11-ownership.md#2.%20Shards) | share | bound | the whole share |
| share's namespace | [RFC 12 §2.1](rfc-12-snapshots.md#2.1%20A%20namespace%20is%20the%20unit%20that%20moves) | share | bound | the share's own |
| oldest unoffloaded extent alert | [RFC 8 §11.4](rfc-8-engine.md#11.4%20How%20far%20behind%20durability%20is%2C%20is%20observable) | share | live | proposed: 1 h |
| snapshot policy, backup location and retention | [RFC 12 §6.2](rfc-12-snapshots.md#6.2%20Configuration) | share | live | none |
| policy `backup.kind`: `catalog` or `copy`, a copying backup | [RFC 12 §3.4](rfc-12-snapshots.md#3.4%20Copying%20backups) | share | live | `catalog` |
| policy `backup.verify_every`: period between verifications of a copying backup; 0 never verifies on a period | [RFC 12 §3.4.3](rfc-12-snapshots.md#3.4.3%20Writing%20one%2C%20step%20by%20step) | share | live | 0 |
| `backups.copy_rate`: copying backups' transfer rate | [RFC 12 §3.4.6](rfc-12-snapshots.md#3.4.6%20Cost%20and%20pacing) | installation | live | proposed: 200 MiB/s |
| `rehome.rate`: a re-home's copy rate; 0 pauses it | [RFC 12 §4.7](rfc-12-snapshots.md#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace) | installation; per share while its re-home runs | live | proposed: 100 MiB/s |
| `snapshots.hold_bound`: held journal bytes per share before a cut is refused | [RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history) | share | live | 64 GiB |
| `snapshots.hold_journal_fraction`: held share of one journal's capacity, summed over every share it carries, before a cut is refused | [RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history) | installation | live | 0.25 |
| `snapshots.reserve`: history bytes per share before a new cut is refused | [RFC 12 §2.9](rfc-12-snapshots.md#2.9%20Space%20is%20reported%2C%20not%20charged) | share | live | none |
| `migration.freeze_timeout`: bound on a move's freeze and drain | [RFC 12 §4.2](rfc-12-snapshots.md#4.2%20The%20move%2C%20step%20by%20step) | installation | live | 5 min |
| group-commit bound `G`, files per existence batch | [RFC 8 §5.2](rfc-8-engine.md#5.2%20Group%20commit%20is%20bounded%2C%20and%20retries%20only%20the%20files%20that%20conflict) | node, per journal | live | proposed: 256 |
| offload retry backoff caps, normal and under capacity pressure | [RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline) | node | live | proposed: 1 s to 60 s; 5 s under pressure |
| speculation budget, bytes in flight per share | [RFC 8 §7.4](rfc-8-engine.md#7.4%20The%20speculator) | share | live | open in RFC 8 |
| read-ahead cap, window ahead of one reader | [RFC 8 §7.4](rfc-8-engine.md#7.4%20The%20speculator) | share | live | open in RFC 8 |
| quota reservation slack `S`, per principal or project per primary | [RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota) | installation | live | open in RFC 17 |
| audit policy: which operations emit an access event | [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) | share | live | none: no access events |
