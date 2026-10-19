---
rfc: 13
title: "RFC 13 — configuration"
component: configuration
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-9-gc]]"
  - "[[rfc-10-journal-replication]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-26-catalog-backups]]"
  - "[[rfc-27-namespace-migration]]"
  - "[[rfc-28-journal-format]]"
aliases:
  - RFC 13
tags:
  - rfc
---
# RFC 13 — configuration

**Status:** draft. [§11](#11.%20Open%20questions) lists what is undecided; [§10](#10.%20Edits%20this%20document%20asks%20of%20other%20RFCs)
records the edits it asked of other RFCs.
**Audience:** anyone adding a setting, implementing the control plane's records,
or deciding what an operator may change and when. Conventions and test tiers are
in [the RFC index](rfc-index.md).

This document specifies behaviour, not the current code.
[Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists where the code differs.

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

Every DittoFS setting lives in one place: records in the metadata store, called the control plane.
A server's own file holds only what it needs to find that place: who it is, where the store is, its local disks.
Each setting also says what changing it does: apply now, apply at the next restart, apply to new data only, or never once data exists.
Think of a building whose plans are kept at the town hall: you may repaint a room any day, but not move a load-bearing wall.
Where the picture breaks: here the town hall refuses the wall change outright, naming the setting and the rule.
A wrong value is refused when it is written, and again when it is used; it is never quietly swapped for a default.
Passwords and keys never sit in a setting; a setting holds only the name of a sealed secret.

**What this is.** Every DittoFS component takes a few settings. This RFC says
where all of them live — in one place, the control plane, kept in the metadata
store — which quantities are settings at all, how far each one reaches, and
above all what happens when one is changed while data is already stored.

**The problem, with one example.** The `profiles` share keeps its content in a
**namespace**, a prefix in the S3 bucket `dfs-data`. Each block's location in
the bucket follows from its name and that prefix, and each chunk is named by a
keyed hash of its bytes under the namespace's chunk-ID key.

An operator tidying the bucket edits the prefix to `dittofs/profiles/` in a
configuration file on the node and restarts it. Nothing fails at once, and:

1. every block already stored is unreachable, because its location follows from
   its name and the old prefix: alice's next sign-in reads her evicted container
   blocks and fails;
2. new blocks land under the new prefix, so the namespace is split across two
   prefixes, and GC listing the old one finds blocks it thinks nothing records;
3. **(cluster)** in a cluster, nodes still reading the old prefix from their own
   files disagree with this one about where the namespace is.

Under this RFC:

1. The prefix is one field of the namespace's record in the metadata store,
   read by every node. No host file can override it.
2. It is **bound**: it decides where stored content is. While the store holds
   any, the control plane refuses the change and names the field and the rule.
   alice's data stays readable.
3. What the operator wants is a migration — a re-home into a new namespace
   ([RFC 27](rfc-27-namespace-migration.md)) — not an edit. `Target`, the chunk-ID key and whether the
   namespace encrypts are bound the same way: each decides the identity of
   stored content.

Other settings of the same share change differently:

- turning on compression, part of the remote store's transform chain, governs
  the **next write**: new blocks are compressed, old ones stay as they were
  written and remain readable, because each block's name records how it was
  written. Turning on encryption is not: whether a namespace encrypts is fixed
  when it is created;
- the `atime` policy is **live**: every node applies a change within 5 s;
- the syncer's worker pool sizes need a **restart**: a pool is sized once, when
  the process builds it;
- the S3 credential is never in configuration at all. The record holds a
  reference to a sealed secret, and rotating the credential changes no record.

![Each host holds only its bootstrap — node identity and roles, how to reach the metadata store, local devices and scratch paths, listen addresses, logging, the memory limit and where its own roles' wrapping keys are — and uses it to reach the control plane in the metadata store, which holds every setting as a record at one of five scopes (installation, node, namespace, remote store, share) with one of four binding classes (live, restart, next write, bound), and secrets only as sealed records referenced by name; an operator writes records through the API or a provisioning file, validated before commit, and each component reads the settings generation every second, runs a live change within 5 s and validates again when built, refusing rather than defaulting an invalid value](img/rfc13-overview.svg)

**The words you need.**

- **control plane** — the records in the metadata store that hold every
  setting; the one source ([§2.1](#2.1%20The%20control%20plane%20is%20the%20source)).
- **bootstrap** — the few facts a host must hold to reach the control plane, and
  nothing else ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)).
- **fixed** — a quantity that is not a setting, because no operator can name a
  workload its value is wrong for; a wrong one is changed in a release
  ([§4.1](#4.1%20Fixed%20by%20default)).
- **scope** — what a setting must be the same across: the
  [installation](rfc-0-data-lifecycle.md#Glossary), a node, a namespace, a remote
  store — the store configuration its namespaces share — or a share
  ([§3](#3.%20Scopes)).
- **binding class** — what a change does: live, restart, next write, or bound
  (refused while content exists) ([§5](#5.%20Binding%20classes)).
- **secret reference** — the name of a secret sealed under the wrapping key of
  the role that uses it; configuration never holds the value ([§7](#7.%20Secrets)).
- **provisioning file** — an optional file of records, applied through the API;
  the API then refuses to edit what it declares ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).

**What this RFC promises.**

- Each setting has one source, the control plane; a host cannot override it.
- An invalid or unknown value is refused, naming the field, when it is written
  and again when the component is built. It is never clamped or replaced by a
  default, and it stops only what it configures.
- A bound setting never changes while content depends on it, and that content
  stays readable.
- A live change reaches every node within 5 s; a node that lags is reported as
  unhealthy.
- A node joins only an installation whose format and protocol versions it
  supports, and no version is raised until every node supports it.
- No secret value appears in any record, API answer, export, log or metric, and
  a node without the `storage` role cannot unseal the remote store's credentials.

**How the rest is organised.** §2 says where configuration lives, including the
backup location's record, §3 the scopes, §4 what is fixed and what is a setting.
§5, the binding classes and the version gate, is the core. §6 is validation and
§7 secrets. §8–§9 are metrics and tests, §10 the edits this RFC asks of others,
§11 what is still open. Appendix A lists where today's code differs, and
Appendix B every setting the RFCs name, with its scope, class and default —
the table to look a setting up in.

### Scenarios

Each runs on N1, serving the `profiles` share from the bucket `dfs-data`.

**An operator changes the `atime` policy of `profiles`.**

1. The change goes through the API, which runs the component's own validator before writing it ([§6](#6.%20Validation)).
2. The same transaction raises one installation-wide counter, the settings generation ([§5](#5.%20Binding%20classes)).
3. Every node reads that counter once a second and re-reads only the records whose generation moved ([§5](#5.%20Binding%20classes)).
4. The setting is live, so every node runs the new policy within 5 s of the commit ([§5](#5.%20Binding%20classes)).

Outcome: no restart, and each node reports which generation of the record it runs ([§2.3](#2.3%20A%20record%20is%20versioned)).

**A provisioning file with a misspelled field is reloaded.**

1. The reload validates the whole file with the API's validator before writing anything ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).
2. The misspelled field is unknown to the schema, so it is refused rather than ignored ([§6](#6.%20Validation)).
3. The whole file is refused; nothing is half-applied ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).
4. The running records stay as they were, and N1 reports a health condition naming the record and the reason ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).
5. Had N1 been starting rather than reloading, it would have refused to start ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).

Outcome: a typo never silently runs the default it was meant to replace.

**The bucket's credential is rotated.**

1. The remote store's record holds a reference to a sealed secret, never the credential itself ([§7](#7.%20Secrets)).
2. Rotating writes the new value under the same reference as a new version; the old version stays usable ([§7](#7.%20Secrets)).
3. The change raises the settings generation, so every node using it picks it up within the live bound ([§7](#7.%20Secrets)).
4. The new credential must prove it reaches the same bucket: the store's check must find the namespace's claim there ([§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change)).
5. Only once every node reports the new version is the old one deleted ([§7](#7.%20Secrets)).

Outcome: no setting changes, and no node is ever left holding a credential that was already deleted.

**N1 is upgraded to a new binary, then rolled back.**

1. At start the new binary registers the format versions it can read and write ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).
2. It opens the store the old binary wrote and keeps writing the old active versions, the ones the installation records as current ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).
3. Until an operator raises a version through the control plane, reinstalling the old binary still works ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).
4. In a cluster, the raise is refused while any registered node lacks the new version, naming the nodes that lag ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).
5. Once a version is raised, the old binary refuses to start rather than misread what it finds ([§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)).

Outcome: a rollback is safe up to the moment the operator raises a version, and refused cleanly after it.

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
  Changing it is a migration ([RFC 27](rfc-27-namespace-migration.md)), never an edit.
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
| the scratch paths beside them: `backups.spill_path`, where copy staging spills ([RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), and `salvage.path`, where stranded journal tags are exported ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)) | a path means something only on its host, and whether it shares space with a journal only the host can tell |
| the process memory limit — the soft limit the runtime enforces (on the Go runtime, `GOMEMLIMIT`) or the container's, else the host's memory | it sizes the node's hold pool ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)), and what memory a process has only its host knows |
| listen addresses                                                                                                      | a port conflict is a host's problem                                                                                                       |
| log destination and level                                                                                             | needed before the control plane answers, to say why it did not                                                                            |
| the node's **roles** — `protocol`, `storage`, both by default ([RFC 15](rfc-15-topology.md))                          | they decide what the node composes, including whether it writes control-plane records, which only `storage` does ([RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)); fixed for the life of the process |
| the location of the wrapping key of each role the node runs, and no other, for `Secret` records ([§7](#7.%20Secrets)) | a key kept in the store it protects protects nothing, and a key for a role the node does not run is a key it can leak                     |

**A scratch path is checked against the journals at every start, in both
directions.** The node refuses `backups.spill_path` or `salvage.path` if it
shares a device, a filesystem or free space with any of its journals — the path
checked against every journal, and every journal, one added since included,
against the path — since a scratch write that fills a journal's space answers
its writers no space ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)). Free space is shared when
the two lie in one filesystem, one pool or one thinly provisioned volume group,
not only on one device. A refused path does not stop the node: a refused spill
path spills nothing, and a refused salvage path fails each export as
[RFC 1 §3](rfc-1-journal.md#3.%20Interface) has it. A value set for the whole
installation could not be checked this way, since one node's scratch disk can
be another's journal filesystem.

A bootstrap fact **MUST NOT** also be a record field, and a record field
**MUST NOT** be overridable from the host. An override from the host is a
second source ([§2.1](#2.1%20The%20control%20plane%20is%20the%20source)): nodes of one installation then disagree, silently.
A provisioning file ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)) is not an override: it writes records through the
control plane, and the record then says it came from the file.

**An installation is created by an explicit initialisation step, never by a
start.** The step mints the installation ID once and passes the same ID to the
store's `Init` ([RFC 16 §3.2](rfc-16-metadata-store.md#3.2%20How%20it%20is%20assembled)), which writes it into the Installation record
beside the store format record ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)), and to each journal's `Init`, which writes it into the journal's `format`
file ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)). A start opens what exists and creates neither: one that
finds journals naming an installation and no metadata store reports **metadata
store lost**, and one that finds a store whose journals are missing, or name
another installation, reports the journal missing or foreign; both wait for an
operator. All evidence that an installation exists is otherwise inside the
store, so a store volume that mounts late would look like a first start. A
start-time option to **initialise if empty** exists for development and tests
only, and **MUST NOT** be on by default.

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
shares, users, groups, grants, quotas, snapshot policies — in the same shape
the management API accepts.

**Applying is a merge through the API.** At start, and on every reload, the node
validates the whole file with the API's validator ([§6](#6.%20Validation)) and then writes
each declared record as the API would: created if absent, replaced if it
differs. Records the file does not declare are left alone. A file that fails
validation, or whose change the API would refuse — a bound setting with content
behind it ([§5.1](#5.1%20A%20bound%20setting%20refuses%20change)), for instance — is refused whole: at start the node refuses to
start, and on reload the running records stay as they were and the node reports
a health condition naming the record and the reason. Nothing is half-applied.
Three cases are reported and the rest applied (table below): a refused prune,
because a deletion's refusal can last as long as a snapshot's lock; a declared
record the control plane retired, such as a namespace and its shares a move
released; and a create-only field whose value differs, such as a re-homed
share's namespace. Each is a state the API legitimately reached, so refusing
the whole file over it would stop a start or a reload that has nothing wrong.

**A declared record has one source, the file.** A file declares a **source
name**, stable across edits, and a **revision**, a number its author raises with
every edit. Each record the file writes is marked **managed**, with the source
name, the revision and the content hash that wrote it. Ownership follows the
source name, never the hash: a hash changes with every edit, and a record owned
by a hash would refuse the next version of its own file. The API **MUST** refuse
to change or delete a managed record, answering that it is declared by that
source and must be changed there and reloaded. This keeps §2.1's rule: every
record has exactly one source, and an operator can see which.

**The refusal covers what the file declares, not what the system records.** A
managed record's fields are of two kinds. **Declared** fields are the ones the
file states; the refusal above guards them. **System** fields are written by the
control plane as a consequence of an operation, never by the file: a backup
location's put-integrity outcome ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record)), a share's namespace generations
and state during a re-home or a move ([RFC 27 §2](rfc-27-namespace-migration.md#2.%20Moving%20a%20namespace%20between%20installations)), a record's generation.
The control plane **MUST** write a system field of a managed record as it would
of any other, and a file that declares one is refused at validation. A
declared field that an operation the API allows must change — a re-home rebinds
a provisioned share to a new namespace — is **create-only**: the file's value
applies when the record is created, and afterwards a file whose value differs
is not applied but reported as a health condition naming the field and both
values, until its author updates the file. So a provisioned share can be
re-homed, and a managed location's check outcome can be recorded, without
giving the API a way to edit what the file owns.

| Event | Effect |
| --- | --- |
| a declared record differs from the store | the file's value is written |
| the control plane deleted a managed record by an operation the API allows — a move's release of a namespace and its shares to another installation, a completed share removal | the deletion records a **retired** marker with the source name, and a reload does not re-create the record: it reports a health condition naming the record and the operation until the file stops declaring it. Re-created, a namespace a move released would be claimed again on a prefix the new owner holds |
| a record is removed from the file | it stays, and stops being managed; with `prune: true` in the file it is deleted instead, subject to the same refusals as an API delete (`DeleteShare` for a share, with the snapshot disposition the file's `prune_snapshots` names — `refuse`, the default, `delete` or `detach` — [RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)). A share already `being removed` or `retired` counts as deleted, so a restart during its removal applies nothing. A refused prune never refuses a start or a reload: the record stays managed and the node reports a health condition naming it and the refusal, since the file is the only source that may delete a managed record |
| an API call edits a declared field of a managed record | refused, naming the file |
| an operation writes a system field of a managed record, or changes a create-only field through an allowed operation | written; for a create-only field the file's differing value is reported, not applied |
| a file declares a record managed by another source name | refused: the record changes source only after the first source stops declaring it; health condition on the refusing node |
| a node applies its source at a revision below the one recorded | refused as stale, so a node restarted with an older copy never reverts a newer edit; health condition naming both revisions |
| a node applies its source at the recorded revision with a different hash | refused: one revision is one content; health condition |
| reload | an explicit reload request through the control-plane API, a signal, or at start; the node reports the applied source, revision and hash ([§8](#8.%20Observability)) |

**Secrets stay references.** A provisioning file **MUST NOT** hold a secret value.
It names one by reference — an environment variable or a file on the host — and
applying seals the value into a `Secret` record ([§7](#7.%20Secrets)) as the API would.

**Node-scoped records** a file declares apply only to the node that reads it.
Every other scope is installation-wide, so in a cluster one node, or an
identical file on every node, provisions it.

> ponytail: a node applies its file only at start and on an explicit reload; it
> does not watch the file. Add a watch when operators edit files in place often
> enough that a forgotten reload shows up as drift.

### 2.5 A backup location is its own record

A backup location ([RFC 26 §2.4](rfc-26-catalog-backups.md#2.4%20Copying%20backups)) is an installation-scoped record,
not a field of a snapshot policy, so its mode, credential and lifecycle are
validated once and shared by every policy that names it. It holds:

| Field | Meaning | Class |
| --- | --- | --- |
| store | endpoint, bucket and prefix, as a remote store's | bound while any backup is held there |
| credential reference | a `Secret` ([§7](#7.%20Secrets)), sealed under the `storage` key | live, under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) |
| inspection credential reference | for `immutable`, optional: a `Secret` of the bucket owner's that may only read the bucket's settings, used where the location's credential cannot read them. The open **MUST** find a bucket-policy deny naming its principal for every action but the reads of the bucket's settings — in its own form: the named principal, a resource covering what its actions act on, and no condition ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) — since an explicit deny binds even the owner's own principals and the credential can read it; a credential its policy would let extend retention, or change the policy, is refused ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) | live, under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) |
| **mode** | `mutable` or `immutable`; required, no default | bound while any backup is held there |
| lifecycle age | for `immutable` only: the age at which the service expires an object, as its expiry rule is configured; the current-version expiry rule's age is at least this ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) | live, within the checks below; required for `immutable` |
| retention cap | for `immutable`, the longest retention a put may set, enforced by the bucket's policy and verified at every open: at least the longest retention of any policy writing there, plus one generation, `backups.max_copy_time` and one day, in whole days ([RFC 26 §2.4.4](rfc-26-catalog-backups.md#2.4.4%20Expiry%20and%20the%20sweep)) | live, within the checks below; required for `immutable` |
| generation `G` | for `immutable`, the period every retain-until is extended past its need, so a reused version is extended about once per `G` ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) | live; proposed: 7 days ([Appendix B](#Appendix%20B%20%E2%80%94%20the%20settings)) |
| put-integrity outcome | for `immutable`, the result of the capability check's integrity step, run once when the record is created and whenever its store or credential changes, since check objects there cannot be deleted | system field ([§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)): written by the control plane, never configured |
| break-glass principal | for `immutable`, optional: the one principal the bucket policy may exempt from the cap and legal-hold denies. The open checks the exemption and both denies by effect, in the one form whose effect it can read off the policy, and the record's creation provokes an assumption of this principal with both credentials and a legal hold with the location's, each to be refused ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)); the control plane **MUST** refuse a record whose break-glass principal is the location's credential or the inspection credential, since a stolen put credential that may place legal holds can lock every version for good ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)). Absent, no principal is exempt, and the open requires both denies to bind every principal | live, within the checks below; default: absent |
| health object identity | the location-level health object at `<location>control/health`, put when the record is created, before any namespace copies there: its version for `immutable`, the nonce written in it for `mutable` ([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)). Creating the record reads the object first, through a get that returns its version, and adopts the version or nonce already there — one object serves every installation that configures the location, so a second installation, a move's target or a recovery import, never overwrites the first's nonce. Every folder's get probe reads it, and a changed endpoint or credential proves the same location by it ([§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change)) | system field, written with the record; when the recorded version is missing at an open, a new one is put and recorded, but only under the endpoint and credential that last proved the identity, so a rotation is still proven and a credential proven since may re-put it |

**An immutable location is proven, not trusted.** The control plane **MUST**
refuse an `immutable` record, and the store **MUST** refuse to open it, unless
its open-time probe finds versioning on, compliance-mode object lock with **no**
default retention — every put sets its own retain-until, and a default long
enough for the longest policy would lock every hourly block as long — a bucket
policy capping the retention a put may set at the record's retention cap and
denying the credential every change to the bucket's policy, lock, lifecycle and
versioning, every principal but the record's break-glass principal, if it
names one, denied a retention beyond the cap and any legal hold, a noncurrent-version and a current-version expiry rule — the latter
no younger than the lifecycle age — over the location's whole root, no
transition to an archive class, and a delete of the health object, naming no
version, refused for its credential; it issues no put as a probe
([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)). A
`mutable` location takes the ordinary capability check
([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)).

**Ages are checked against each other.** For an `immutable` location, a policy
that writes there is refused when its `retain` plus `backups.max_copy_time`
exceeds the lifecycle age — `retain` starts when a copy completes, so a block
put at the start of a long copy must outlive the copy and the retention — and
when its `retain` plus one generation, `backups.max_copy_time` and one day
exceeds the retention cap; a `mutable` location has neither field. At any
location a policy is refused with `ErrPolicyPeriod` when its period is shorter than its estimated incremental
copy at the configured copy rate ([RFC 26 §2.4.4](rfc-26-catalog-backups.md#2.4.4%20Expiry%20and%20the%20sweep)). A first copy is not held to
the period: it completes across attempts, each building on the last.

**A location in use is not deleted.** Deleting a location record **MUST** be
refused with `ErrLocation`, naming the backups, while any backup there is
neither `expired` nor `lost`: a master key's destroy check reads only the locations the
installation has a record for ([RFC 26 §2](rfc-26-catalog-backups.md#2.%20Catalog%20backups)), so a deleted record would
hide the backups that check protects. Nor while the location is pending in
an export key's rotation: deletion first makes the location's `keys` put and
reads it back, or, if it cannot, refuses, so a retiring key is never left
waiting on a location no re-seal can reach
([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)); a lost location
leaves every pending set with its lost mark.

**A location lost for good is declared, not deleted.** A bucket that is gone
can neither be read nor have its backups expired, so the refusal above would
hold its record, and every master key its exports name, forever. An operator
**MAY** state that a location is lost, through one audited control-plane call
naming the location. Its first transaction writes a durable **lost mark** on
the location record; from that commit a master key's destroy check skips the
location ([RFC 26 §2](rfc-26-catalog-backups.md#2.%20Catalog%20backups)). Then, in batches within the
transaction key bound, resumed from the mark after a crash, it marks every
backup there `lost`, deletes their use records, and empties the census and the
block count of every folder record at the location, so the namespaces that
copied there can drain and their material can retire
([RFC 9 §7.6](rfc-9-gc.md#7.6%20A%20namespace%20no%20share%20names%20is%20drained)): a lost bucket's folder records could
never be settled by a sweep, which must read the bucket. A location of 10⁴
shares' retained backups holds far more records than one transaction may
write, so the work is batched, not one transaction. Once the last batch has
committed, `ErrLocation` no longer refuses deleting the record, since every
backup there is `lost`. The statement is the operator's, not a
probe's: an outage, however long, never makes one, since a location that
answers again would hold backups the store had stopped protecting.

## 3. Scopes

A setting has exactly one scope, the smallest thing it must be the same across:

| Scope | Holds | Examples |
| --- | --- | --- |
| **installation** | what every node and namespace shares | the syncer's pool sizes' defaults, GC's schedule |
| **node** | what describes one host's resources | its journals, one per device, and each one's maximum footprint ([RFC 1 §6](rfc-1-journal.md#6.%20Capacity)) |
| **namespace** | what decides stored bytes and their identity, and where they sit | prefix, chunk-ID key, chunking key, data keys, `Target`, whether the chain encrypts, the counting domain |
| **remote store** | the store configuration every namespace created on it shares: how blocks reach and sit on a service ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)) | endpoint, bucket, credential reference, storage class, transform chain, block target, material provider, master keys |
| **share** | what a client sees | case sensitivity, name length, `atime`, per-share journal limit, snapshot policy |

![The five scopes nested inside one installation: node N1 holds its journals and their footprints; a remote store configuration holds endpoint, bucket dfs-data, credential reference, storage class, transform chain, block target, material provider and master keys, and is shared by several namespaces, each opening its own store under its own prefix with its own prefix, chunk-ID key, chunking key, data keys, Target, encryption choice and counting domain; shares such as profiles hang off a namespace and hold what a client sees; bootstrap facts, fixed and derived values have no scope; what decides a chunk's boundaries or identity is a namespace setting because deduplication compares chunks across a namespace](img/rfc13-scopes.svg)

**A setting that decides stored bytes belongs to the scope content is compared
across.** Deduplication compares chunks across a namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)), so
whatever decides a chunk's boundaries or its identity is a namespace setting. A
share-level `Target` would let two shares of one namespace cut the same file
differently and stop deduplicating each other without saying so.

**A namespace has one store configuration, which many namespaces may share.**
Each namespace opens its own per-namespace store under its own prefix from that
configuration ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)), so the prefix is a namespace setting and
everything that reaches the service is the configuration's. The transform chain
is a configuration setting ([RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration)), and so the same for every share of
every namespace on it. Whether the
chain encrypts is the one part that is bound and recorded with the namespace at
its creation ([§5.1](#5.1%20A%20bound%20setting%20refuses%20change)); the rest of the chain is next write.

**How a scope is written.** Every setting has exactly one of the five scopes
above. [Appendix B](#Appendix%20B%20%E2%80%94%20the%20settings) writes it in one of three forms, and no other:

- *scope* — one value across that scope;
- *scope*, **per** *thing* — one value for each instance of a thing inside that
  scope, such as `node, per journal`; the thing is never itself a scope;
- *scope* **default**, *narrower scope* **override** — a value at the wider
  scope that one narrower record **MAY** replace, such as a replica count with
  an installation default and a per-shard value in the shard record. The two are
  one setting, validated by one rule.

A quantity that is **fixed** or **derived** ([§4.1](#4.1%20Fixed%20by%20default)), or a **bootstrap** fact
([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)), has no scope, and Appendix B says so in its class column instead.

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

- a remote store's endpoint, bucket and credential reference; a namespace's
  prefix;
- whether a namespace's chain encrypts;
- a backup location's mode, and for an `immutable` one its lifecycle age and
  retention cap ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record)); a `mutable` location takes neither, since no
  bucket rule there is checked against them;
- a node's journal devices;
- every store configuration's material provider and master key, whether or not
  its chain encrypts, since every namespace's export key is wrapped under a
  master key ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)); and, for a master key held in a key file,
  its verified off-host escrow: until the provider's escrow check has passed,
  creating an encrypting namespace or a backup policy on that configuration is
  refused with `ErrNotEscrowed`, naming the key.

Everything else takes a default. [Appendix B](#Appendix%20B%20%E2%80%94%20the%20settings) lists the settings the RFCs
name, each with its default. A default marked **proposed** there is a starting
value awaiting a measurement, whether its owning RFC or this document supplies
it; every other default is settled.

## 5. Binding classes

What changing a setting does is a property of the setting, declared with it:

| Class | A change takes effect | Examples |
| --- | --- | --- |
| **live** | on the next operation that reads it, on every node | snapshot schedule, GC interval, `atime` policy |
| **restart** | when the process next starts | pool sizes ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)), listen addresses |
| **next write** | for content written from then on; stored content keeps what it was written with and stays readable | the compression stage and its settings ([RFC 5 §5.1](rfc-5-transforms.md#5.1%20Configuration%20governs%20the%20next%20write)), storage class, block target |
| **bound** | never, once content exists | chunk-ID key, chunking key, `Target`, whether the chain encrypts, bucket, prefix, case sensitivity and entry-digest key, a backup location's mode |

![The four binding classes side by side: live changes reach every node within 5 s through the settings generation read every second; restart changes apply when the process starts, and values nodes must agree on at every instant are restart, held old and new during a cluster rolling restart; next write changes govern only new content, recorded with it through the chain ID in each block name and the census, so old blocks stay readable; bound settings such as bucket, prefix, chunk-ID key, Target and whether the chain encrypts are recorded at creation, compared at every open, and refused while content exists, a change being a migration; a new endpoint or rotated credential for the same bucket is not a change but must be proven by finding the namespace claim](img/rfc13-binding-classes.svg)

**A live change reaches every node within 5 s.** Every change to a setting or
`Secret` record raises one installation-wide **settings generation**, in the
same transaction. Each node reads that one record every second — one point read,
which any backend answers without a watch ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)) — and re-reads
the records whose generation moved. A node **MUST** run a live change within 5 s
of its commit; the one-second read leaves the rest of the bound to a slow store. While a change spreads, nodes **MAY** run
different generations of one record; each reports the generation it runs
([§8](#8.%20Observability)). A setting whose nodes **MUST** agree at every instant — a node
lease, the drift bound — is therefore not live: it belongs to the **restart**
class, where the change is taken up by a planned restart of every node.

**(cluster)** **A restart-class value nodes must agree on changes in two
steps.** During a rolling restart some nodes run the old value and some the new,
so the record holds both until every node reports the new generation. Each node
restarted meanwhile runs under the safer of the two in each direction: it waits
out the longer of the two timeouts it waits on another node for, and promises
the shorter of the two intervals it renews within, so a lease one node grants
under the old value never outlasts what another fences under the new. Once every
node reports the new generation, the control plane drops the old value, and the
next restart of each node runs the new one alone.

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
([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), the one sanctioned change of that binding: it copies the
share's content into a new namespace while the share serves, and rebinds it only
once no ref names the old one. What an operator wants from such a change is a
migration — a re-home into a new namespace, or a move between installations
([RFC 27 §2](rfc-27-namespace-migration.md#2.%20Moving%20a%20namespace%20between%20installations)) — which leaves the old content where it can still be read
until the move completes.

### 5.2 Reaching the same content another way is not a change

Some fields name how to reach content, not where it is: a store's endpoint, and
its credential. A new endpoint for the same bucket, or a rotated credential, is
allowed, and is proven rather than trusted: the store's capability check
([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)) **MUST** find, at the new location, what identifies the old one.
For a namespace's store that is the namespace claim the old one held
([RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)). A backup location holds no claim, so for it that is its health
object's identity in its record ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record)): the recorded version, read by
version, at an immutable location, or the recorded nonce in the object's body at
a mutable one. A location that answers but holds another claim or identity, or
none, is a different store, and the change is refused.

### 5.3 What content was written with is recorded with it

A **next write** setting is recorded with the content it governed, where a later
reader or a census can find it: the chain ID in every block name, the census in
every block record ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)). A **bound** setting is recorded once, with
the namespace, when it is created, and compared with configuration at every
open: a namespace whose recorded `Target` differs from its record
does not open ([RFC 8 §2.4](rfc-8-engine.md#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced)); so does one whose chunk-ID key fingerprint, or
whether it encrypts, differs from what was recorded at its creation. Reading never consults configuration for how
content was written ([RFC 5 §2.5](rfc-5-transforms.md#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)).

### 5.4 A format version is written only once every reader reads it

The block format version a namespace writes ([RFC 4 §3.5](rfc-4-remote-tier.md#3.5%20Format%20changes%20are%20migrations)) is a **namespace
setting**, next write, defaulting to the newest version every node of the
installation reads. The control plane **MUST** refuse to advance it past the
oldest version any node that may read the namespace supports, and a node
**MUST** refuse to join while a namespace it would serve writes a version it
cannot read. A newer binary alone therefore never writes blocks an older one,
still serving, would refuse.

### 5.5 A node joins only where its versions overlap

Every format and protocol a node shares with others is versioned: the metadata
store format ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)), the journal format ([RFC 28 §2](rfc-28-journal-format.md#2.%20On-disk%20format)), the block
format ([§5.4](#5.4%20A%20format%20version%20is%20written%20only%20once%20every%20reader%20reads%20it)), the settings record schema ([§2.3](#2.3%20A%20record%20is%20versioned)), and **(cluster)** the
messages nodes exchange. For each, the installation records one **active
version**, the one written, and each node registers the range it can read and
write, at every start, in its node record.

- **A node refuses to start** in an installation, and a binary refuses to open a
  store, when any active version lies outside the range it registers. It names
  the format and both versions.
- **An active version is raised only when every registered node reports the new
  version in its range**, and the control plane **MUST** refuse the raise
  otherwise, naming the nodes that lag. A node that is down still counts, since
  it may start again with its old binary; one that will not return is
  **decommissioned** by the operator, which deletes its node record, and no
  longer counts. The raise is one transaction, on the installation record or,
  for the store format, on the store format record `\x00format`, its only record
  ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)); a node learns it through the settings generation ([§5](#5.%20Binding%20classes)).
- **A downgrade is possible only while every active version lies in the older
  binary's range**; once a version has been raised past it, the older binary
  refuses to start rather than misread what it finds.

So a rolling upgrade is: upgrade every node's binary, each still writing the old
active versions; then raise each active version, which the control plane allows
only once the last node runs the new binary. On a single node the same gate runs
at start: a new binary opens a store written by the old one and keeps writing
the old active versions; it raises one only through the same control-plane
call, never at open. Until that call the old binary can still be started on the
same store, so a single node rolls back by reinstalling it.

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

**A rule across settings is checked from both sides.** Some rules relate two
settings, such as `smb_pending_cap` exceeding `migration.freeze_timeout` plus
the break deadline ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). A write of
either setting **MUST** be validated against the current value of every setting
a rule relates it to, in the same transaction as the write, reading those
values tracked so that a concurrent write of the other side conflicts. A rule
stated in an RFC names its settings in [Appendix B](#Appendix%20B%20%E2%80%94%20the%20settings),
so the validator knows which writes re-check it.

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

![Secret records in the metadata store are sealed per role: a protocol-only node holds only the protocol wrapping key, so it unseals password hashes, NT hashes, keytabs and identity-provider bind credentials but cannot open the storage-sealed remote-tier credentials it reads; a storage node unseals those with the storage key; rotating the bucket credential seals a new version under the same reference while the old stays resolvable, raises the generation so each node re-resolves within 5 s, proves the value and reports it, waits until every node reports the new version, then deletes the old one and reports it may be revoked](img/rfc13-secrets.svg)

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
	// Delete destroys every version of the secret; a later Resolve of ref
	// fails.
	Delete(ctx context.Context, ref SecretRef) error
}
```

- **Read never returns a secret.** No API, export, log line or metric carries
  one. An export that must travel with its secrets, a backup that must be
  restorable ([RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material)), carries references, and says which secrets the
  destination must hold.
- **A secret changes without a record change, in two phases.** Rotating a
  credential writes a new value under the same reference, as a new version of
  the `Secret` record; the previous version stays resolvable. The change raises
  the settings generation ([§5](#5.%20Binding%20classes)), so every node using the reference
  resolves it again within the live bound and reports the version it now holds
  ([§8](#8.%20Observability)). Only once every such node reports the new version does the
  provider delete the old one, and the control plane report that the old
  credential may be revoked at its service; revoking it earlier fails the nodes
  that still hold it. The new value is proven as [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) proves a new
  credential before it is used.
- **A user's secrets go with the user.** `DeleteUser`'s last transaction, the
  one that deletes the user record, deletes every version of each `Secret`
  that record names — its password hash and NT hash among them
  ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
  Nothing else names those secrets, so a later pass could not find them, and a
  password-equivalent left behind outlives the account in the store and every
  copy of it.
- **A wrapping key rotates the same way.** A node's bootstrap **MAY** name two
  keys for one role, the current and the next. Re-sealing every `Secret` of that
  role under the next key is one pass; each record names the key ID it is sealed
  under, and the current key is removed from bootstraps only once no record
  names it.
- **Material is a secret with a lifecycle.** Encryption keys are resolved through
  the material provider, which also tracks which keys exist, are current or are
  destroyed. A secret provider need not.
- **Which keys exist is RFC 5's table, not this document's.**
  [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) is the one statement of the keys each kind of namespace
  has, what wraps each, which rotate and which must live off the host. This
  document adds only where configuration names them: a remote store's
  configuration references its **master keys**, which the provider holds and
  never releases; a namespace's record holds each of its keys as (material ID,
  fingerprint), never the key; the provider refuses an ID whose fingerprint
  changed. In the terms of that table:
  - a **master key** is held where [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) says,
    including when a key file's off-host escrow must be verified first, which
    [§4.2](#4.2%20Every%20setting%20has%20a%20default%2C%20or%20is%20required) enforces and this list does not restate, and rotates by
    re-wrapping, which changes no record. A master key that any retained export
    names, or that a move pins, is never destroyed, and a destroy that cannot
    read a backup location is refused ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)) unless an
    operator has declared that location lost ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record));
  - every namespace key — data, chunking, export, and an encrypting
    namespace's chunk-ID key — is wrapped only under a master key of its store
    configuration. **No namespace key is wrapped under the `storage` wrapping
    key**: that key lives on the host, so a backup wrapped under it would be
    restorable only on the host that made it, the one place a backup is not
    needed;
  - a non-encrypting namespace's **chunk-ID key** is held in the clear, in its
    key record and beside its blocks, so losing the host loses none of its
    content; its keyed IDs hide nothing, since its bodies are plaintext. Its
    exports are still sealed under its export key, which is wrapped under a
    master key like every other: **restoring any namespace's metadata from a
    backup needs the master key**, and the bucket alone does not suffice;
  - every key record reaches the namespace's `keys` object in its bucket before
    it becomes current, and a backup location's before any state, export or
    progress version is put there, so a recovery finds keys rotated after the
    last backup ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys));
  - **data keys and export keys rotate**: a data key by
    new material plus relocation ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)), an export key by issuing a new
    current one, of a higher generation, that later exports are sealed with,
    the old one retired only once every backup location has been re-sealed
    under the new one ([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)). Each is a **next write**
    setting, not bound. The chunk-ID and chunking keys never rotate: changing
    either renames or re-cuts every chunk, which is a re-home
    ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)).
- **An export names its keys and is sealed under them.** It carries the
  namespace's key records as that table holds them, the IDs of the master keys
  that wrap them and of its current and every older (retiring or retired)
  export key, with the digests of the retained exports sealed under an older
  key ([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)), so a forgery under a
  retired key is refused, and is encrypted and authenticated under the export key ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)).

- The only secrets on a host are the bootstrap credential for the configuration
  store and the wrapping keys of its own roles, or references to them in an
  external key service ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)).

## 8. Observability

Metric names are shown without the deployment's prefix, which the exporter adds.

| Answers | Metric | Type |
| --- | --- | --- |
| the generation of each record a node runs, against the store's current one | `config_generation` | gauge, labelled by record |
| the version of each secret a node holds, against the current one | `config_secret_version` | gauge, labelled by reference |
| each format's active version, and the range each node registers | `format_active_version`, `format_supported_range` | gauges, labelled by format |
| records refused, by component and reason | `config_refused_total` | counter |
| changes refused because a setting is bound | `config_bound_refusals_total` | counter |
| the provisioning file hash each node applied, and applies refused by reason | `config_provisioning_applied`, `config_provisioning_refused_total` | gauge, counter |

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
| [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap) explicit initialisation | Start a node against an empty metadata store and empty journal directories: assert it refuses, creating nothing. Initialise: assert the store's Installation record and every journal's `format` file carry one installation ID. Start with the store volume unmounted: assert it reports the metadata store lost and creates nothing; with the journal directory emptied, the journal missing. Only with initialise-if-empty set does a start on empty storage create an installation. |
| [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record) location deletion | Delete a location record holding one unexpired backup: refused `ErrLocation`, naming it. Expire it; delete again: accepted. Make a second location unreachable for good with one backup there: deletion refused. State it lost, crashing after the lost mark and after the first batch: assert the statement resumes from the mark, the backup reads `lost` in the store, its folder record's census and block count are empty, the statement is in the audit log, deletion is accepted, and a master key only that backup's export named can be destroyed. Leave a third location unreachable for a day without the statement: still refused. |
| [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record) break-glass principal | Create an `immutable` record naming a break-glass principal whose policy exempts exactly it: accepted. Exempt a second principal as well, or name the location's credential or the inspection credential as the break-glass one: refused. Omit the field with a policy that exempts anyone: refused. Let the break-glass role's trust admit the installation's account, or scope the legal-hold deny to the bucket rather than its objects, or add a condition to it: each refused. |
| [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record) health object re-put | Rotate an `immutable` location's credential and prove it; make the health object's recorded version missing — on a fault-injecting test service, since no principal can delete a compliance-locked version before its retain-until; open: assert a new version is put under the rotated credential and recorded. Open under a credential never proven: refused, nothing put. |
| [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) backup location credential | Rotate an immutable and a mutable backup location's credential: assert both accepted, proven by the health object's recorded version and nonce. Point the record at another bucket: refused. A design that looks for a namespace claim at a backup location refuses every rotation. |
| [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap) scratch paths | Start a node with `backups.spill_path` on a journal's filesystem, then on a second subvolume of the journal's pool, then on a separate filesystem and add a journal on it before the next start; assert each is refused at start, the node serves, nothing spills, and a stranded tag's export with `salvage.path` refused the same way is a failed export reported as a health condition. Set either path as a record field: refused. |
| [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap) no host override | Set a record field in the host's environment. Assert it is refused at start, not applied. |
| [§2.3](#2.3%20A%20record%20is%20versioned) schema | Present a record with a newer schema version. Assert refused. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) provisioning | Start a node with a file declaring a namespace, a share and a setting. Assert all three exist and are marked managed. Edit the share through the API; assert refused, naming the file. Change the file and reload; assert applied. Remove the share from the file; assert it stays, unmanaged; add `prune: true`; assert deleted. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) all or nothing | Reload a file with one valid change and one invalid record, then one changing a bound setting with content behind it. Assert nothing changed, the health condition names the record, and at start the node refuses to start. Move a declared namespace away and reload the unchanged file with one valid change: assert the change applied and the retired record reported, not re-created and not refusing the file. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) secrets | Declare a remote store whose credential is an environment variable. Assert the file holds no value, the record references a sealed `Secret`, and no API or log shows the value. |
| [§5.4](#5.4%20A%20format%20version%20is%20written%20only%20once%20every%20reader%20reads%20it) format version | Run two nodes, one reading one format version fewer. Advance the namespace's version to write; assert refused. Upgrade the second node; assert accepted, and every block either node writes afterwards reads on both. |
| [§7](#7.%20Secrets) namespace keys | Replace a namespace's chunking key under the same ID. Assert the provider refuses it by fingerprint and the namespace does not open. |
| [§7](#7.%20Secrets) chunk-ID key | Create two namespaces without encryption and store one file's bytes in each. Assert the chunk IDs differ between them and differ from the unkeyed hash of the bytes. A design that keys chunk IDs only when encrypting fails. |
| [§5.1](#5.1%20A%20bound%20setting%20refuses%20change) encryption bound | Turn encryption on for a namespace that holds content. Assert refused. Turn compression on; assert accepted, and old blocks still read. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) source, not hash | Apply a file, edit it and raise its revision, reload. Assert applied — a design that owns records by hash refuses its own edit. Restart a second node with the earlier revision; assert refused as stale and no record reverted. Apply the same revision with different content; assert refused. |
| [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record) immutable location | Point an `immutable` location at a bucket with versioning but no object lock; assert the record is refused. Add compliance-mode lock, noncurrent- and current-version expiry over the whole root, and a bucket policy holding the retention cap and denying the five configuration actions; assert accepted, that the record holds the health object's version, and that the open issued one delete naming no version, saw it refused, and issued no put. Drop the current-version rule, or one configuration deny; assert refused. Add a bucket default retention; assert refused. Remove the cap policy, or raise it past the record's cap; assert refused at open and at `Recheck`. Give a policy a `retain` plus `max_copy_time` above the lifecycle age, or a `retain` plus one generation and `max_copy_time` above the cap; assert refused. Give a 40 TiB share a daily policy at 200 MiB/s whose increment fits a day: assert accepted, though its full copy does not; shrink the period below the increment: refused `ErrPolicyPeriod`. |
| [§2.4](#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) system fields | Provision a share and an immutable location from a file. Re-home the share: assert it runs, the share's namespace changes, and the next reload reports the file's stale namespace as a health condition without applying it. Create the location: assert its put-integrity outcome is recorded. Declare the outcome in the file: refused at validation. A design that refuses every write to a managed record fails both. |
| [§5](#5.%20Binding%20classes) rolling restart class | **(cluster)** Raise the node lease from 10 s to 20 s and restart three nodes one at a time. Assert each restarted node waits out 20 s before taking over a lease and renews within the old interval until every node reports the new generation, and that no lease was served by two nodes at any instant. Apply the new value on each node as it restarts instead: a takeover by a restarted node falls inside a lease an old node still holds. |
| [§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap) dead node | Stop one of three nodes for good and raise an active version: assert refused, naming it. Decommission it: assert the raise is accepted. On a single node, upgrade the binary without raising; assert the old binary still starts on the store. |
| [§7](#7.%20Secrets) key table | Create a non-encrypting namespace: assert its chunk-ID key is in the clear in its key record and its key object, and that no namespace key of any namespace is wrapped under the `storage` key. Delete the host's wrapping keys and restore its catalog backup on a fresh host holding the master key: assert every file reads back. Create an encrypting namespace on a key-file master key whose escrow is not verified: refused `ErrNotEscrowed`. Rotate a data key and an export key of a namespace holding content: assert accepted, old blocks and exports still read. Destroy a master key a retained export names: refused. |
| [§5](#5.%20Binding%20classes) propagation | Change a live setting on a store backend that offers no watch. Assert every node runs the new generation within 5 s. Freeze one node's reads; assert the health condition names it after 5 s. Move a serving share from `standard` to `bounded` with no restart: assert a record written 5 s after the commit is synced within the sync bound. |
| [§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap) version gate | Run two nodes; register a third whose range excludes an active version; assert it refuses to start, naming the format. Upgrade one node's binary; raise an active version; assert refused, naming the other node. Upgrade it; assert the raise is accepted. Start the old binary; assert it refuses. A design without a recorded active version accepts the first raise. |
| [§7](#7.%20Secrets) user secrets | Create a user with a password, rotate it once, delete the user. Read the store's raw keys: assert no version of either secret remains, and a restart mid-`DeleteUser` still leaves none once it resumes. |
| [§7](#7.%20Secrets) rotation | Rotate a credential while two nodes use it. Assert both report the new version within 5 s, the old version stays resolvable until both do, and only then is it deleted. Rotate a wrapping key with both keys in every bootstrap; assert every `Secret` names the new key ID before the old key can be removed. |
| [§3](#3.%20Scopes) scope forms | Parse Appendix B's scope column. Assert every entry is one of the five scopes in one of §3's three forms, or "—" with a class of fixed, derived or bootstrap. |
| [§6](#6.%20Validation) block target range | Set the block target to 512 KiB and to 128 MiB. Assert both refused, naming the field and the range; assert 1 MiB and 64 MiB accepted. |

## 10. Edits this document asks of other RFCs

Each owning RFC is the authority for what its settings mean; this document is
the authority only for their scope and class, and where the two disagree the
disagreement is a defect in one of them, fixed there, not settled by
precedence. Every edit this document asked of another RFC is applied, dropped
because the rule stands here alone, or marked not yet applied:

1. **RFC 2 §3.2:** `Target` is a namespace setting, bound. *Applied.*
2. **RFC 2 §6 and RFC 5 §3.2:** "a share that encrypts" reads "a namespace that
   encrypts"; the chunking key is the namespace's. *Applied.*
3. **RFC 4 §4.2 and Appendix C:** bucket and prefix bound; endpoint and
   credential change under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change). *Dropped:* RFC 4 states no class for them,
   so nothing there contradicts [§5.1](#5.1%20A%20bound%20setting%20refuses%20change), [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) and Appendix B, which carry
   the rule.
4. **RFC 12 §3.2:** its example is the control-plane records it describes, with
   the scopes of Appendix B: `snapshots.hold_bound` and `snapshots.directory`,
   which Appendix B scopes per share, sit under the share. *Applied.*
5. **RFC 7 §3.3:** case sensitivity is fixed at the share's creation. *Applied.*
6. **RFC 9 §8:** GC's `Config` holds the six namespace `gc.*` settings of
   Appendix B, `gc.hold_threshold` among them. *Applied.*
7. **RFC 2 §5:** the block target is a remote-store setting, next write, 4 MiB,
   settable from 1 MiB to 64 MiB. *Applied.*
8. **RFC 5 §3.2 and §5.1:** whether the chain encrypts is fixed when the
   namespace is created. *Applied.*
9. **RFC 26 §4.2 and RFC 12 §3.2:** a policy names a backup location by its record
   ([§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record)). *Applied.*
10. **RFC 16:** the node record holds its registered version ranges; the store
    format's active version is in the store's format record and every other
    active version in the installation record; a `Secret` holds its version and
    the wrapping key it is sealed under, and keytabs are `Secret` records.
    *Applied* ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities), [§4.6](rfc-16-metadata-store.md#4.6%20Store%20format)).
11. **RFC 16:** the installation-wide settings generation of [§5](#5.%20Binding%20classes) is
    RFC 16's `CFGGEN` record, raised in the transaction of every `Setting` or
    `Secret` change. *Applied.*

## 11. Open questions

1. **Proposed defaults.** Every default Appendix B marks proposed is a
   starting value awaiting a measurement, and each row names the measurement
   that settles it, or carries a decision that none will. Each stands until that
   measurement, wherever it runs, replaces it with the value it selects, under
   the index's rule for setting defaults ([index, Test tiers](rfc-index.md#Test%20tiers)).
2. **Values open in their own RFCs.** The segment size, a fixed constant
   ([RFC 28 §5](rfc-28-journal-format.md#5.%20Open%20questions), question 2), the GC partition count
   ([RFC 9 §12](rfc-9-gc.md#12.%20Open%20questions), item 6); and
   **(cluster)** the gather interval, replica removal triggers, mark
   persistence, re-read period and repair pacing ([RFC 10 §16](rfc-10-journal-replication.md#16.%20Open%20questions)). Each takes a
   default here only once its RFC states one.
3. **The settings generation read.** One point read per node per second is
   nothing on one node; whether it stays cheap with many nodes on one replicated
   store, or needs the store's own change feed, is unmeasured **(cluster)**.

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

Every setting the storage RFCs name, with its scope and class. A default marked
"proposed" is a starting value awaiting a measurement, which every such row names
after "settled by"; the first reference-box run replaces it under the index's rule
([Test tiers](rfc-index.md#Test%20tiers)), read for a setting default as the
value its row's settling measurement selects, wherever that measurement runs —
per service, per client OS, from traces — not as a measured value plus headroom,
which fits a benchmark target only. A default not marked proposed is settled,
and where no measurement could move it the row says why. A proposed value is not a
requirement, and RFC text cites the setting rather than restating its number as one.
A row still reading "open in RFC n" has no starting value yet; its owning RFC
supplies one before the setting is built.

| Setting | Defined in | Scope | Class | Default |
| --- | --- | --- | --- | --- |
| journal devices and paths | [RFC 1](rfc-1-journal.md) | — | bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)) | required |
| journal maximum footprint | [RFC 1 §6](rfc-1-journal.md#6.%20Capacity) | node, per journal | live | proposed: 80% of the device; settled by: write refusals and repack bytes per byte freed under the profile workload with the footprint at 70%, 80% and 90% of the device |
| per-share journal limit | [RFC 1 §6](rfc-1-journal.md#6.%20Capacity) | share | live | the journal's maximum: no per-share limit until an operator sets one, a policy rather than a measurement |
| segment size | [RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments) | — | fixed: a constant of the implementation, never a setting | value open in RFC 28 §5, question 2 |
| `sync.policy`: `bounded`, every written record synced within the sync bound by the journal's timer, or `standard`, synced only when a client asks, the timer off | [RFC 1 §5.2](rfc-1-journal.md#5.2%20Sync%20policy) | share | live: a change is handed to the journal through `SetSyncPolicy`, per stream, and applies to every record written after it ([RFC 1 §5.2](rfc-1-journal.md#5.2%20Sync%20policy)), within the live bound ([§5](#5.%20Binding%20classes)) | proposed: `bounded`; settled by: J1 without a `Sync` after each write under both policies, syncs per second and throughput against the data a crash loses ([RFC 1 §5.2](rfc-1-journal.md#5.2%20Sync%20policy)'s decision) |
| sync bound: under `bounded`, longest a written record waits for a sync | [RFC 1 §5.2](rfc-1-journal.md#5.2%20Sync%20policy) | — | fixed | proposed in RFC 1: 1 s; settled by: J1 without a `Sync` after each write, at 1 s and at a longer bound ([RFC 1 §5.2](rfc-1-journal.md#5.2%20Sync%20policy)) |
| `ExtentLimit`: placement-index entries per journal | [RFC 1 §4.2](rfc-1-journal.md#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes) | node, per journal | restart | proposed: 2²⁶ entries (about 3.4 GB at 51 bytes each); settled by: index memory in [RFC 1](rfc-1-journal.md)'s soak against the reference box's memory, the bound set so every journal of a node fits beside the process's other budgets |
| index excess: how far removals and releases may take the placement index above `ExtentLimit` before a release or deallocate that would split a held extent is refused retry-later | [RFC 1 §4.2](rfc-1-journal.md#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes) | — | derived | an eighth of `ExtentLimit`; eviction releases whole extents, which split nothing, so the cap never wedges a full journal |
| drop ranges per (file, segment): past it the two closest ranges merge, keeping the lower pair | [RFC 1 §7.2](rfc-1-journal.md#7.2%20Repack) | — | fixed | 8; settled, since a merge only keeps a removal longer and never drops one, so the value trades memory against repack's cost, never safety; its ponytail names the evidence that would replace it |
| append streams per journal | [RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments) | node, per journal | restart | proposed: 16; settled by: J1's throughput across stream counts with 16 concurrent writers |
| `DescriptorLimit`: segment descriptors held open per process | [RFC 1 §7.4](rfc-1-journal.md#7.4%20Open%20descriptors) | node | restart | proposed: a quarter of the process's open-file limit; settled by: reopen rate under the mixed read load of [RFC 1](rfc-1-journal.md)'s soak |
| segments one offer may pin; segments all offers may pin together | [RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload) | — | fixed | proposed in RFC 1: 64; 256; settled by: a journal held at its limit under offload ([RFC 1 §10](rfc-1-journal.md#10.%20Open%20questions), question 4), whether offers ever wait on the bound |
| `journal.headroom_records`: header-only records reserved for removal, loss, hold and stamp records | [RFC 1 §6](rfc-1-journal.md#6.%20Capacity) | node, per journal | restart | proposed: 65,536 (14 MiB at 224 bytes each: the largest header-only record, a clone target, with its footer entry); settled by: a journal held at its limit under a removal-heavy load, `headroom_draws` against the headroom's size, and whether removals ever reach the refusal ([RFC 1 §10](rfc-1-journal.md#10.%20Open%20questions), question 4) |
| `journal.idle_seal_bytes`: idle-seal threshold | [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding) | node, per journal | restart | proposed: 16 MiB; settled by: J6, the share of time to first read spent scanning active-segment tails, against footer writes from early seals under a trickle load |
| idle-seal time: an active segment with no append for this long is sealed once past `journal.idle_seal_bytes` | [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding) | — | fixed | proposed in RFC 28: 30 s; settled by: J6, as `journal.idle_seal_bytes` |
| repack reserve: journal space outside every share's limit for repack's copies | [RFC 1 §7.2](rfc-1-journal.md#7.2%20Repack) | — | fixed | at least one segment's live payload ([RFC 0 §8.2](rfc-0-data-lifecycle.md#8.2%20Reclaim)) |
| `Target` | [RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it) | namespace | bound | proposed: 256 KiB; settled by: [RFC 2 Appendix C](rfc-2-carver.md#Appendix%20C%20%E2%80%94%20choosing%20the%20target)'s plan through the real offload path, before the first namespace binds it |
| key scope | [RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope) | — | derived | the namespace ID's 16 raw bytes, never configured; it never changes, because the namespace ID does not |
| chunk-ID key (material kind `chunk-id-key`) | [RFC 0 §2.1](rfc-0-data-lifecycle.md#2.1%20Entities), [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) | namespace | bound | created with the namespace, never configured; wrapped under a master key when the namespace encrypts, in the clear beside its blocks when it does not ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)) |
| export key (material kind `export-key`): seals and authenticates the namespace's exports and state objects | [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys), [RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout) | namespace | next write: a new current key seals later exports; a retired one stays until no retained export names it | created with the namespace, never configured; wrapped under a master key, by (ID, fingerprint) ([§7](#7.%20Secrets)) |
| `encrypts`: whether the namespace's chain has an encrypt stage, recorded at creation | [RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration) | namespace | bound | required |
| chunking key | [RFC 2 §6](rfc-2-carver.md#6.%20Boundaries%20are%20public) | namespace | bound | drawn at random at creation when the chain encrypts; wrapped under a master key, by (ID, fingerprint) ([§7](#7.%20Secrets)) |
| data key, current | [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) | namespace | next write | created with the namespace when the chain encrypts; wrapped, by (ID, fingerprint) ([§7](#7.%20Secrets)) |
| master keys | [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) | remote store | live: rotates by re-wrapping; never destroyed while a retained export names it | required on every store configuration, held where [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys) requires; a key file needs verified escrow where B.2 says (`ErrNotEscrowed`); references only ([§7](#7.%20Secrets)) |
| block format version to write | [§5.4](#5.4%20A%20format%20version%20is%20written%20only%20once%20every%20reader%20reads%20it) | namespace | next write | the newest version every node reads |
| `upload_workers`, `fetch_workers` | [RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed) | installation | restart | proposed: 128 each, or the sizing tool's figure; settled by: the sizing tool's put curve (upload) and ranged-get curve (fetch) per supported service ([RFC 4 §7.5](rfc-4-remote-tier.md#7.5%20Benchmarks%20and%20targets)), and B5 at that size |
| slow-tail retry share: most slow-tail retries per store and direction, as a share of its transfers over a fixed window | [RFC 3 §2.4](rfc-3-syncer.md#2.4%20Every%20transfer%20terminates%2C%20and%20reports) | — | fixed | proposed in RFC 3: 5% over 60 s; settled by: the slow-tail retry rate on each supported service under the sizing tool's load, and a store slowed fourfold, against first-attempt latency ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)) |
| retry budget demand reserve; throttled-attempt bound | [RFC 3 §2.4](rfc-3-syncer.md#2.4%20Every%20transfer%20terminates%2C%20and%20reports) | — | fixed | proposed in RFC 3: a quarter of each bucket; eight throttled attempts per transfer; settled by: a copying backup and a reader's cold fetches sharing one failing path, and a throttling service's burst replies |
| endpoint, credential reference | [RFC 4 §4.1](rfc-4-remote-tier.md#4.1%20Interface) | remote store | live, under [§5.2](#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change) | required |
| bucket | [RFC 4 Appendix C](rfc-4-remote-tier.md#Appendix%20C%20%E2%80%94%20the%20S3-compatible%20block%20store) | remote store | bound | required |
| prefix | [RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside) | namespace | bound | required |
| storage class | [RFC 4 §8](rfc-4-remote-tier.md#8.%20Decisions%20and%20open%20questions) | remote store | next write | the service's default |
| block target | [RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler) | remote store | next write | proposed: 4 MiB; settable from 1 MiB to 64 MiB, and a value outside is refused; settled by: the block-size benchmark against each service before release ([RFC 4 Appendix B](rfc-4-remote-tier.md#Appendix%20B%20%E2%80%94%20measurements)) |
| transform chain but its encrypt stage: compression and its settings, `require` | [RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration) | remote store | next write | empty |
| material provider | [RFC 5 §2.5](rfc-5-transforms.md#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material) | remote store | live | required on every store configuration |
| compress stage `with_encryption: accept`: compression in a chain that encrypts | [RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration) | remote store | next write | absent: a chain with an encrypt stage and a compress stage without it is refused |
| compress stage `level` | [RFC 5 Appendix A](rfc-5-transforms.md#Appendix%20A%20%E2%80%94%20compression) | remote store | next write | proposed: 3 (zstd's own default); settled by: [RFC 5 §8.3](rfc-5-transforms.md#8.3%20Benchmarks%20and%20targets)'s compression ratio and MB/s per core on the three corpora, at levels 1 to 9 |
| case sensitivity: the share's fold rule, by ID from the store format record ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)) | [RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case) | share | bound; an export carries it ([RFC 26 §2.1](rfc-26-catalog-backups.md#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)) | sensitive (identity rule) |
| entry-digest hash key: orders a directory's entries and keys its listing cookies | [RFC 7 §3.4](rfc-7-namespace-metadata.md#3.4%20Enumeration) | share | bound; an export carries it ([RFC 26 §2.1](rfc-26-catalog-backups.md#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)) | drawn at the share's creation, never configured |
| xattr value bound, xattrs per file, named streams per file | [RFC 7 §2.7](rfc-7-namespace-metadata.md#2.7%20Extended%20attributes%20and%20named%20streams) | share | live: a change applies to later writes; existing ones stay readable | 64 KiB, Linux's own value bound; 1024; 1024. Compatibility bounds, not tuning, so no measurement settles them |
| `atime` policy | [RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps) | share | live | relative, Linux's default; a compatibility choice, not a measurement |
| `gc.interval`: between compaction and collection passes | [RFC 9 §8](rfc-9-gc.md#8.%20API%20surface) | namespace | live | proposed: 1 h; settled by: [RFC 9 §11.6](rfc-9-gc.md#11.6%20Benchmarks%2C%20scale%20and%20soak)'s backlog-drain and deletion-storm rows, the interval at which a pass's work stays within the deletion storm's p99 bound |
| `gc.trash_retention`: time a retired block with recoverable chunks waits before its delete | [RFC 9 §3.7](rfc-9-gc.md#3.7%20Trash) | namespace | live | proposed: 48 h; settled by: how long after its deletion a defect the audit finds is still recoverable, against the bytes held in trash under a deletion storm ([RFC 9 §11.6](rfc-9-gc.md#11.6%20Benchmarks%2C%20scale%20and%20soak)) |
| GC clock skew limit: largest offset of a deleter's clock from store time before it stops | [RFC 9 §3.7](rfc-9-gc.md#3.7%20Trash) | — | fixed | proposed in RFC 9: 1 min; settled by: measured skew on supported hosts under ordinary time sync |
| GC partitions per namespace | [RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix) | — | fixed | open in [RFC 9 §12](rfc-9-gc.md#12.%20Open%20questions), item 6 |
| `gc.space_amp_target`: stored over referenced bytes the compactor holds the namespace under; 0 turns compaction off | [RFC 9 §4.4](rfc-9-gc.md#4.4%20When%20to%20compact%20is%20policy) | namespace | live | proposed: 1.25; settled by: [RFC 9 §12](rfc-9-gc.md#12.%20Open%20questions) item 1, a churning workload's amplification under each target against the bytes each rewrites |
| GC `Recheck` period: between re-reads of a namespace store's settings and claim | [RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period) | — | fixed | proposed in RFC 9: 5 min; settled by: the service calls one `Recheck` costs across every namespace of an installation, against how long a revoked credential may go unseen ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)'s decision) |
| lease durations: GC partition lease, a snapshot use record's deadline | [RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix), [RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot) | — | fixed: constants of the implementation | proposed: 60 s for a GC partition lease, 10 min for a use record's deadline, each renewed at a third of its length; settled by: the longest pause a holder shows in the soak runs ([RFC 9 §11.6](rfc-9-gc.md#11.6%20Benchmarks%2C%20scale%20and%20soak)), with the lease at least ten times it |
| backup folder record's lease *L* | [RFC 26 §2.4.1](rfc-26-catalog-backups.md#2.4.1%20Layout%20at%20the%20location), [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) | — | derived | max(10 min, 2σ), renewed at a third of its length: hosts on different installations judge it by their own clocks, and its ceiling holds whenever *L* exceeds about 1.62σ, so raising σ lengthens it with no other change; the 10 min floor is settled as the use record's deadline is |
| `gc.audit.period`: time within which the audit covers every chunk and block record; its rate is derived from it | [RFC 9 §6.1](rfc-9-gc.md#6.1%20Coverage) | namespace | live | proposed: 7 days; settled by: the projected time to cover 2 PB at measured audit throughput ([RFC 9 §11.6](rfc-9-gc.md#11.6%20Benchmarks%2C%20scale%20and%20soak)'s coverage row), which selects the shortest period that throughput holds, against how long a defect may go unseen |
| `gc.audit.forward_period`: time within which a full forward pass covers every ref; incremental passes cover the refs changed since the last pass, and only a full pass lets a count be lowered | [RFC 9 §6.1](rfc-9-gc.md#6.1%20Coverage) | namespace | live | proposed: 90 days; settled by: [RFC 9 §12](rfc-9-gc.md#12.%20Open%20questions) item 2, the full pass's point-read rate against how long a leaked count may hold its block |
| `gc.hold_threshold`: low-count mismatches within one audit period that set the namespace's hold | [RFC 9 §6.4](rfc-9-gc.md#6.4%20A%20store-wide%20trip%20holds%20the%20deleter) | namespace | live | proposed: 16 per `gc.audit.period`; settable from 1 to 1,024, a value outside refused; settled by: mismatch counts from the audit run on a real store at steady state, the threshold sitting above the background rate of isolated defects |
| replica count **(cluster)** | [RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement) | installation default, shard override (in its shard record) | live | proposed: 3; settled by: the durability target against measured node-loss and repair-time distributions |
| replica floor **(cluster)** | [RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement) | installation default, shard override (in its shard record) | live | proposed: 2; settled by: the exposed-window length from the repair-time distribution |
| failure domain **(cluster)**: the host, rack or zone a node lies in | [RFC 10 §7.1](rfc-10-journal-replication.md#7.1%20Count%2C%20floor%20and%20placement), [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | node | live | the node itself |
| replication gate **(cluster)**: opened by an operator once every node runs a binary that knows the journal extension | [RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension) | installation | live, opened only under [§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)'s rule; never closed again | closed |
| stall bound **(cluster)**: how long a sync or the serving loop may make no progress before a node stops renewing; a shard whose replication stalls is relinquished instead, and stops no renewal ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) | [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch) | installation | live | proposed: 5 s; settled by: the longest healthy journal-sync and serving-loop stall under load, against the takeover time it would trigger |
| node lease duration **(cluster)** | [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch), [RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) | installation | restart ([§5](#5.%20Binding%20classes)) | proposed: 10 s; settled by: the takeover-time benchmark ([RFC 10 §15](rfc-10-journal-replication.md#15.%20Test%20plan%20and%20benchmarks)) against the rate of self-fences on store hiccups shorter than the lease |
| node lease renewal interval **(cluster)** | [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch), [RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) | — | derived | a third of the node lease (3.3 s at its proposed default), not set apart from it, so no pair of values can leave an idle node self-fencing before each renewal |
| drift bound **(cluster)** | [RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) | installation | restart | proposed: 500 ms; settled by: measured clock offset and rate between nodes and the store on the reference deployment |
| gather interval: **(cluster)** how long a claimant waits for replicas' committed points | [RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| replica removal triggers: **(cluster)** removal bound, lag in bytes, lag in time | [RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| replica mark persistence period **(cluster)** | [RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| removed-replica re-read period: **(cluster)** how often a replica re-reads the shard records it holds content for | [RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| repair pacing: **(cluster)** joins in flight per node and per cluster | [RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair) | installation | live | open in [RFC 10 open question 4](rfc-10-journal-replication.md#16.%20Open%20questions) |
| repair scheduler enabled **(cluster)** | [RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair) | installation | live | on |
| open-state lease: NFSv4 lease period, SMB durable-handle timeout | [RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease) | — | fixed | NFSv4 90 s; SMB per the protocol |
| `smb_pending_cap`: longest an SMB request refused for a transient cause — capacity, a store stall, the version limit, quota slack, `ErrUnavailable`, or a metadata conflict (`DelayConflict`) — is held `STATUS_PENDING` before it is answered: `STATUS_DISK_FULL` for capacity, `STATUS_UNEXPECTED_IO_ERROR` for a store stall, the version limit, quota slack or `ErrUnavailable`, `STATUS_FILE_NOT_AVAILABLE` for a metadata conflict still unresolved (`DelayConflict`); a request held for a frozen or quiesced share is bounded by `migration.freeze_timeout` plus the break deadline instead ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)) | [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values), [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here) | installation | live | proposed: 10 min, must exceed `migration.freeze_timeout` plus the SMB break deadline; settled by: the longest capacity refusal or freeze observed to clear under a fill-and-drain test, against the SMB clients' tolerance of a pending request |
| `smb_pending_deadline`: longest deadline an SMB request held `STATUS_PENDING` on a recall may carry; must exceed the SMB break deadline ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) | [RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks) | installation | live | proposed: 60 s; settled by: the time each client OS (Windows, macOS, Samba) tolerates a pending request before cancelling or reconnecting, against the distribution of break-resolution times under a slow-client test |
| `adapter.drain_deadline`: longest a draining adapter waits for requests already handed to the service | [RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states) | installation | live | proposed: 90 s, at least the SMB break deadline plus the request deadline; settled by: the longest held request a drain must let finish |
| `adapter.idle_timeout`: a connection with no request and no held state is closed | [RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits) | installation | live, for connections accepted afterwards | proposed: 15 min; settled by: clients' reconnect cost against idle connection state |
| `adapter.write_deadline`: longest one message may wait to be written before its connection is closed | [RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits) | installation | live, as above | proposed: 60 s |
| `adapter.handshake_deadline`: negotiate and authenticate, from accept | [RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits) | installation | live, as above | proposed: 30 s |
| `adapter.frame_deadline`: the rest of a request once its first byte has arrived | [RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits) | installation | live, as above | proposed: 30 s |
| `adapters.nfs.tls`: `cert_file`, `key_file`, `client_ca`, `min_version`, `mode` (`opportunistic`, `require`) | [RFC 20 §5.5](rfc-20-adapters.md#5.5%20Message%20transforms) | installation | live: certificate files re-read on change, each field for handshakes or connections begun afterwards | off; `min_version` 1.3, `mode` opportunistic |
| operations switched off, per adapter, by name | [RFC 20 §4.2](rfc-20-adapters.md#4.2%20The%20operation%20table) | installation | live | none |
| sidecar enabled: `mdns`, `wsd`, `portmap`, `rpcbind` | [RFC 20 §3.5](rfc-20-adapters.md#3.5%20Sidecars) | installation, node override | live | as the current release ships them |
| `clone_max_len`: longest `CLONE` or duplicate-extents request answered as one atomic clone; a longer one is `ErrInvalid` | [RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally), [RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone) | installation | live | proposed: 1 GiB; settled by: the time one atomic clone of that length takes on the reference box, against the default request deadline and the store's transaction limits ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)) |
| copy chunk: the length one atomic clone of a `COPY` or copychunk covers; the copy runs as a series of them and may answer a short count | [RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally), [RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone) | — | fixed | proposed: 64 MiB; settled by: the longest a writer to the source waits behind one chunk on the reference box, against the copy's throughput ([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)'s decision) |
| version block: versions one durable raise of a share's version limit covers; the next raise runs on [RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)'s trigger, evaluated at each draw: the larger of half a block and the peak rate × the margin, 30 s + `existence_age` + 1 s (the trigger's own sampling), the peak being the highest count of versions drawn in any one second of the last 60 s | [RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward) | — | fixed | proposed: 2²⁴ versions, so the half left when a raise falls due covers about 260,000 versions a second for the 32 s margin at the proposed `existence_age`; the 60 s peak window and the 1 s sampling are fixed with it ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)); settled by: stall tolerance first — writes still acknowledged through a metadata stall of the self-fence's length at the reference box's peak version rate — then versions a crash skips, which the 64-bit counter absorbs |
| object bound: largest versioned object put at a backup location | [RFC 4 §4.15](rfc-4-remote-tier.md#4.15%20Versioned%20objects%20at%20a%20backup%20location), [RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout) | — | derived | the largest encoded block, so every versioned object, an export part included, is scheduled as a block is. An export is cut into parts at it, and a head that would exceed it is cut into head parts the same way, listed by a small root ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)), so no export is too large to put |
| retry window **(cluster)**: how long a mutation's request ID and result are kept to answer a retry — the dedup table, a journal request entry, a request record | [RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope) | installation | live: a change applies to entries written after it | proposed: 15 min, above a Linux hard-mount client's retransmission back-off, which starts at 60 s and caps at 600 s; a value below 600 s plus the takeover bound — the node lease, plus the drift bound, plus one renewal period, plus the gather interval ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) — is refused; settled by: retry traces of hard-mounted NFS clients across a takeover and a front-end loss, against the entries the window keeps at the peak mutation rate (about 1.8×10⁷ at 20,000 a second) |
| dedup entries per client **(cluster)**: most entries the dedup table keeps for one client within the retry window, the oldest dropped first | [RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope) | — | fixed | proposed: 65,536; settled by: the same retry traces as the retry window, the most mutations one client sends between a call and its last retransmission at the peak per-client rate. A retry whose entry the cap dropped is applied again ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)'s decision) |
| hold budget per transport: calls and bytes the service may hold at once for one counter key, (front-end node ID, its node epoch, session or connection), the epoch keeping a restarted front-end's new numbering apart from a dead process's held calls, counted for every call that carries a hold policy from its first hold or `ErrDelay` until its context ends | [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values), [RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone) | — | derived | NFSv4.1: `SlotBudget` max(1, a quarter of the session's slots), keyed by session, no byte budget, a hard cap; SMB: `SlotBudget` 256 and `ByteBudget` 32 MiB per connection, constants, a floor past which a connection draws on the node's hold pool, a sixteenth of the process memory limit (a bootstrap fact, [§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)), derived; a test sizes the pool by setting that limit, 128 MiB giving 8 MiB ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)); a call past a hard cap, or past a floor with the pool full, is refused with `DelayHoldBudget`, answered `STATUS_INSUFFICIENT_RESOURCES` over SMB and `NFS4ERR_DELAY` over NFS; NFSv3, NFSv4.0 and NLM pass no policy. An absent policy or a budget of 0 sets no cap, and the request deadline still bounds every hold. In a cluster each primary counts on its own, so a session can exceed its budget by the number of primaries it reaches ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)'s ponytail) |
| default request deadline, for an operation that arrives with none | [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values) | — | fixed | 30 s |
| single-node self-fence: time with a write transaction outstanding or failing and none committed before a node stops acknowledging writes | [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) | — | fixed | 30 s, the default request deadline |
| small-call bound: longest one Small attempt, its one request or one read-then-write | [RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface) | — | fixed | proposed in RFC 3: 30 s; settled by: the small-body cap at a working link's floor share, moved together with the cap ([RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)'s decision) |
| small-body cap: largest body one Small call carries, either direction | [RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface) | — | fixed | proposed in RFC 3: 256 KiB; settled by: as the small-call bound, with which it moves |
| grant budget: delegations, leases and oplocks one client holds at one primary before recalls begin | [RFC 14 §5.5](rfc-14-open-state.md#5.5%20A%20client%27s%20grants%20are%20bounded) | — | fixed | proposed in RFC 14: 4096, recalled down to 2048; settled by: [RFC 14 §5.5](rfc-14-open-state.md#5.5%20A%20client%27s%20grants%20are%20bounded)'s decision, grants held by the profile workload's busiest client |
| clock-rate bound ρ: how far one clock's rate may differ from another's over a wait | [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) | — | fixed | proposed: 0.05; settled by: measured monotonic-clock rates of supported hosts against each other and the store's time |
| clock-offset bound σ: how far two hosts' clocks may read apart when a wait starts | [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) | installation | restart | 1 s; raised by a deployment whose time synchronisation cannot hold two hosts within it. It has no ceiling: every lease fenced by its holder's own clock is derived from it, the folder record's lease as max(10 min, 2σ), so raising σ lengthens those leases with it ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). The cluster's lease waits do not use it ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) |
| grant fence: how long a client that let a recall or break time out is offered no grant | [RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) | — | fixed | 90 s, one lease period |
| SMB break deadline: an oplock or lease break not acknowledged by it times out | [RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) | — | fixed | 35 s, Windows' value; MS-SMB2 leaves the timer to the implementation |
| settings generation read period | [§5](#5.%20Binding%20classes) | — | fixed | 1 s |
| live propagation bound: a live change runs on every node within it, and a node behind it is unhealthy | [§5](#5.%20Binding%20classes) | — | fixed | proposed: 5 s; settled by: commit-to-run time of a live change across nodes on a store with no watch ([§9](#9.%20Test%20plan)'s propagation row) |
| active version of each shared format and protocol | [§5.5](#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap) | installation | live, raised only under §5.5's gate | the version the installation was created with |
| shard slot count **(cluster)** | [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | installation | bound, from the installation's creation | proposed: 4096; settled by: slot-table size and rebalance skew at the largest expected node count |
| capacity weight **(cluster)** | [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | node | live | 1, every node equal; an operator sets it for unequal hardware, so no measurement settles it |
| shard policy: per share, or **(cluster)** subtree or per child, and the directories that start a shard | [RFC 11 §2](rfc-11-ownership.md#2.%20Shards) | share | live: a change applies to files created from then on, and existing files change shard only by a batched move ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) | per share: the whole share |
| `shard.follow_writer`, `.window`, `.share` **(cluster)** | [RFC 11 §3.3](rfc-11-ownership.md#3.3%20The%20primary%20follows%20the%20writer) | installation | live | on; proposed: 5 min; 0.9; settled by: forwarded-byte shares observed on reference workloads |
| `shard.dwell` **(cluster)** | [RFC 11 §3.3](rfc-11-ownership.md#3.3%20The%20primary%20follows%20the%20writer) | installation | live | proposed: 30 min; settled by: handover cost (tail shipped) against measured writer-alternation periods |
| `shard.replace_delay`: **(cluster)** the re-placement delay | [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) | installation | live | proposed: 10 min; settled by: the distribution of node return times after a lapse (restarts, reboots) |
| `shard.unreachable_delay`, `shard.unreachable_handover_bound`: **(cluster)** how long front-ends may fail to reach a primary that reaches the store before its shards are handed away, and how long that handover may take before it becomes a failover | [RFC 11 §5.1](rfc-11-ownership.md#5.1%20Front-ends%20forward%20to%20the%20primary) | installation | live | proposed: 30 s; 60 s; settled by: the client stall a deployment tolerates against brief front-end losses, and handover times of the share limit's tail |
| share's namespace | [RFC 12 §2.1](rfc-12-snapshots.md#2.1%20A%20namespace%20is%20the%20unit%20that%20moves) | share | bound | the share's own |
| oldest unoffloaded extent alert | [RFC 8 §11.4](rfc-8-engine.md#11.4%20How%20far%20behind%20offload%20is%2C%20is%20observable) | share | live | proposed: 1 h; settled by: the oldest-unoffloaded age's p99 under the profile workload with a healthy store, the alert set above it |
| snapshot policy: schedule, retention and the backup location it names | [RFC 12 §3.2](rfc-12-snapshots.md#3.2%20Configuration) | share | live | none |
| backup location: store, credential reference, inspection credential reference, mode, lifecycle age, retention cap, generation `G`, break-glass principal; system fields put-integrity outcome and health object identity | [§2.5](#2.5%20A%20backup%20location%20is%20its%20own%20record), [RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes), [RFC 26 §2.4.4](rfc-26-catalog-backups.md#2.4.4%20Expiry%20and%20the%20sweep) | installation | per field, as §2.5 states | mode required; for `immutable`, lifecycle age and retention cap required, since each is checked against the bucket's own rules and no default fits every policy, and the inspection credential and break-glass principal absent by default; `G` proposed: 7 days, settled by: extension calls a day against the service's request rate and bill (the immutable-location check of [RFC 26 §6.3](rfc-26-catalog-backups.md#6.3%20Group%20A%20%E2%80%94%20lost%20or%20wrong%20content)); the rest as the store's |
| policy `backup.kind`: `catalog` or `copy`, a copying backup | [RFC 26 §2.4](rfc-26-catalog-backups.md#2.4%20Copying%20backups) | share | live | `catalog` |
| policy `backup.verify_every`: period between verifications of a copying backup; 0 never verifies on a period | [RFC 26 §2.4.3](rfc-26-catalog-backups.md#2.4.3%20Writing%20one%2C%20step%20by%20step) | share | live | 0 |
| `backups.copy_rate`: copying backups' transfer rate | [RFC 26 §2.4.6](rfc-26-catalog-backups.md#2.4.6%20Cost%20and%20pacing) | installation | live | proposed: 200 MiB/s; settled by: [RFC 26 §6.5](rfc-26-catalog-backups.md#6.5%20Benchmarks%20and%20targets)'s daily copying backup, against the location's put throughput and live p99 latency |
| `rehome.rate`: a re-home's copy rate; 0 pauses it | [RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace) | installation default, share override while its re-home runs | live | proposed: 100 MiB/s; settled by: [RFC 27 §5.5](rfc-27-namespace-migration.md#5.5%20Benchmarks%20and%20targets)'s re-home under 64 writers, p99 write latency at that rate |
| `snapshots.hold_bound`: held journal bytes per share before a cut is refused | [RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history) | share | live | proposed: 64 GiB; settled by: dirty bytes a share carries at a cut in [RFC 12 §5.5](rfc-12-snapshots.md#5.5%20Benchmarks%20and%20targets)'s cut-to-`complete` benchmark |
| `snapshots.hold_journal_fraction`: held share of one journal's capacity, summed over every share it carries, before a cut is refused | [RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history) | installation | live | proposed: 0.25; settled by: the same benchmark, against the journal headroom [RFC 1 §6](rfc-1-journal.md#6.%20Capacity) leaves for live writes |
| `snapshots.reserve`: history bytes per share before a new cut is refused | [RFC 12 §2.9](rfc-12-snapshots.md#2.9%20Space%20is%20reported%2C%20not%20charged) | share | live | none |
| `snapshots.gate_max`: longest a cut gate stays closed, its drain included | [RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) | installation | live | proposed: 1 s; settled by: [RFC 12 §5.5](rfc-12-snapshots.md#5.5%20Benchmarks%20and%20targets)'s cut under a sustained writer, the gate's p99 at one shard and 64 shards under sustained offload |
| `snapshots.cut_deadline`: a cut not committed this long after its announce is aborted | [RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) | installation | live | proposed: 5 s; settled by: announce-to-commit time of a cut across many shards, from the same benchmark |
| `snapshots.lock_max`: longest lock a snapshot may carry past the time it is set | [RFC 12 §2.7](rfc-12-snapshots.md#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks) | installation | live | proposed: 8760 h (one year); a policy, not a measurement: the longest retention a compliance policy asks for, which no benchmark moves |
| `snapshots.reserve_fraction`: default reserve, history bytes per byte charged live | [RFC 12 §2.9](rfc-12-snapshots.md#2.9%20Space%20is%20reported%2C%20not%20charged) | installation | live | proposed: 1.0; settled by: history growth under hourly snapshots of a churny share (the reserve check of [RFC 12 §5.4](rfc-12-snapshots.md#5.4%20Group%20B%20%E2%80%94%20cost%20and%20wedging)) |
| `snapshots.directory`: the browse directory's name | [RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot) | share | live | `.snapshot` |
| snapshot ordinal quarantine after a deletion | [RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot) | — | fixed | 24 h |
| consecutive skipped policy ticks before a health condition | [RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) | — | fixed | 3 |
| incomplete files a cut may list per shard; a shard with more refuses the close | [RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) | — | fixed | 16 |
| `migration.freeze_timeout`: bound on a move's freeze, through B's ready | [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step) | installation | live | proposed: 5 min; settled by: [RFC 27 §5.5](rfc-27-namespace-migration.md#5.5%20Benchmarks%20and%20targets)'s move of a 10⁷-file namespace, freeze to B serving, against key count |
| move pause deadline: how long past A's last renewal a move's pause record lapses, after which the scheduler resumes or aborts the move; a recovery import's pause has none and is never aborted by it | [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step) | — | fixed | proposed: 10 min, renewed at a third of its length; settled by: the longest pause A shows during a pre-seed in [RFC 27 §5.5](rfc-27-namespace-migration.md#5.5%20Benchmarks%20and%20targets)'s move, with the deadline at least ten times it |
| `migration.reconnect_wait`: how long a moved persistent open is kept at the new owner, from when it begins serving, if its own client makes no tree connect to its share there; the open's own timeout starts at that client's (its `ClientGuid`'s) first tree connect to its share at the new owner, so one client reconnecting starts no other client's timer (decision: per client, not from the first tree connect by any client, which would start the timers of opens whose clients have not yet repointed and close their handles early); it also bounds the move grace a named address holds the moved shares in at B from publish, which ends at the earlier of this and the address's arrival plus one lease. That grace is NFS-only: it refuses non-reclaim NLM locks and NFSv4 opens and locks and accepts their reclaims, and SMB on the moved shares sees no grace, its persistent handles keeping this per-client timer ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step), [RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)) | [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step) | installation | live | proposed: 1 h; settled by: the longest repoint-to-reconnect time the move test's clients show ([RFC 27 §5.5](rfc-27-namespace-migration.md#5.5%20Benchmarks%20and%20targets)) |
| `migration.hold_reply`: longest a call waits in a freeze before `ErrDelay` | [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step) | installation | live | proposed: 1 s; settled by: the longest wait NFS clients in the support matrix tolerate before their own retry |
| staging budget: raw transfers held between fetch and upload by every copy and by GC's relocation, one pool per process, in memory and then spilled to disk | [RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound), [RFC 26 §2.4.6](rfc-26-catalog-backups.md#2.4.6%20Cost%20and%20pacing) | — | derived | as [RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound) derives it, from the copy flow's cap in the fetcher's pool; staging above it spills under `backups.spill_path`, and a reservation waits once memory plus spill reach `backups.spill_max`, or at the budget when no spill path is set; the spill is cleared at start |
| `backups.spill_path`: directory staging spills to above its memory budget | [RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound) | — | bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)) | none: unset, nothing spills and staging waits at the memory budget. Checked against the journals at every start in both directions and refused if it shares space with one ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)); a deployment choice, not a measurement |
| `salvage.path`: directory stranded journal tags are exported to | [RFC 1 §3](rfc-1-journal.md#3.%20Interface) | — | bootstrap ([§2.2](#2.2%20A%20host%20holds%20only%20its%20bootstrap)) | none: unset, every export fails and is reported, and the tags stay stranded. Checked as `backups.spill_path` is; a deployment choice, not a measurement |
| `backups.spill_max`: memory plus spill at which a staging reservation waits for staging to drain, one total per process across every copy and GC's relocation ([RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound)) | [RFC 26 §2.4.6](rfc-26-catalog-backups.md#2.4.6%20Cost%20and%20pacing) | installation | live | proposed: 64 GiB; settled by: the spill [RFC 26 §6.5](rfc-26-catalog-backups.md#6.5%20Benchmarks%20and%20targets)'s daily copying backup reaches from a source faster than its folder |
| `backups.max_copy_time`: bound on one copy attempt; a successor builds on a failed one | [RFC 26 §2.4.3](rfc-26-catalog-backups.md#2.4.3%20Writing%20one%2C%20step%20by%20step) | installation | live | proposed: 48 h; settled by: the longest incremental copy attempt in [RFC 26 §6.5](rfc-26-catalog-backups.md#6.5%20Benchmarks%20and%20targets)'s daily copying backup at `backups.copy_rate`, which selects the value, since an attempt cut short fails into a successor that builds on it rather than losing its work; a first copy spans as many attempts as it needs, so it does not size this |
| `existence_age`: longest a synced write's existence waits for the group commit | [RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal) | installation | live | proposed: 1 s; settled by: the sync bound it matches under the `bounded` policy ([RFC 1 §5.2](rfc-1-journal.md#5.2%20Sync%20policy)), and E5/E7's existence transactions per second at 1 s and shorter. A `standard` share has no sync bound; its cut syncs the share's streams with the gate open, so the gate covers only the residue ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), and this row assumes that pre-sync |
| `group_commit.max_files` (`G`): files per existence batch | [RFC 8 §5.2](rfc-8-engine.md#5.2%20Group%20commit%20is%20bounded%2C%20and%20retries%20only%20the%20files%20that%20conflict) | node, per journal | live | proposed: 256; settled by: E5/E7, transactions per stability point rising at the bound |
| `offload.retry_min`, `offload.retry_max`, `offload.retry_max_pressure`: offload retry backoff, its floor and its caps normal and under capacity pressure | [RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline) | node | live | proposed: 1 s; 60 s; 5 s; settled by: failing-store and recovered-store runs, the cap neither hammering a failing store nor leaving a recovered one idle |
| `offload.max_age`: longest a file's dirty bytes wait below the dirty-byte threshold before it is offered | [RFC 8 §6.2](rfc-8-engine.md#6.2%20When%20a%20file%20is%20offered) | share | live | proposed: 5 s; settled by: a streaming workload (E1/E6), whether the age bound, not the byte bound, fragments blocks |
| consecutive failed offload passes before a per-file health condition | [RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline) | — | fixed | 5 |
| speculation budget, bytes in flight per share | [RFC 8 §7.4](rfc-8-engine.md#7.4%20The%20speculator) | share | live | proposed: 256 MiB, four readers' read-ahead caps; settled by: E4 and E8 with 1, 4 and 16 concurrent readers, against write refusals while speculation runs |
| `readahead.max_blocks`, `readahead.max_bytes`: read-ahead cap, window ahead of one reader | [RFC 8 §7.4](rfc-8-engine.md#7.4%20The%20speculator) | share | live | proposed: 8 blocks or 64 MiB, whichever is smaller; settled by: E4 sequential bytes fetched per byte read and E8 first-byte latency across caps |
| cold-read threshold between fetching chunks and fetching the block | [RFC 8 §7.8](rfc-8-engine.md#7.8%20A%20cold%20read%20asks%20for%20chunks%2C%20or%20for%20the%20block) | — | fixed | a quarter of the block |
| `eviction.repack_threshold`: unreclaimed fraction of a journal's capacity that requests a repack | [RFC 8 §10.3](rfc-8-engine.md#10.3%20Repack%20is%20triggered%20here) | node, per journal | live | proposed: 10 %; settled by: repack bytes copied per byte freed, and write refusals, under the profile workload across thresholds |
| eviction occupancy target: allocated occupancy, under the journal's maximum, that eviction frees down to | [RFC 8 §10.1](rfc-8-engine.md#10.1%20Eviction%20is%20chosen%20here%2C%20and%20needs%20no%20new%20record) | node, per journal | live | proposed: 85% of the maximum footprint; settled by: repack bytes copied per byte freed under the profile workload ([RFC 8 §10.1](rfc-8-engine.md#10.1%20Eviction%20is%20chosen%20here%2C%20and%20needs%20no%20new%20record)), across targets |
| fill low-water mark: free journal capacity, reserved for writes, below which a demanded extent is not filled and pre-warm pauses | [RFC 8 §7.3](rfc-8-engine.md#7.3%20Filling%20is%20a%20decision) | node, per journal | live | proposed: 10% of the maximum footprint; settled by: [RFC 8 §16](rfc-8-engine.md#16.%20Open%20questions) item 1, hit rate and write refusals for fill-always, this rule and fill-never on a large-file workload |
| write pacing: soft threshold *S*, rate window, rate floor *r*₀ | [RFC 8 §10.2.1](rfc-8-engine.md#10.2.1%20Writes%20are%20paced%20before%20the%20limit%2C%20not%20stopped%20at%20it) | — | fixed | proposed: half of the limit *L*; 10 s; 1 MiB/s; settled by: E3's submission latency and offloaded throughput across thresholds and floors |
| quota reservation slack `S`, per principal or project per primary | [RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota) | installation | live | proposed: 1 GiB; settled by: one principal's write throughput over `existence_age` on the reference box, below which its writes wait for a commit ([RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)) |
| quota slack fraction `f`: a quota's slack is `min(S, f × Hard.Bytes)` | [RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota) | installation | live | proposed: 1 %; settled by: the overshoot of a 1 GiB quota written through two primaries against its write latency, across fractions |
| audit policy: which operations emit an access event | [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks) | share | live | none: no access events |
