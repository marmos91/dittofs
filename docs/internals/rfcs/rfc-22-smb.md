---
rfc: 22
title: "RFC 22 — SMB"
component: smb
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-27-namespace-migration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-18-identity]]"
  - "[[rfc-19-authorization]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-21-nfs]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-25-observability]]"
  - "[[rfc-28-journal-format]]"
aliases:
  - RFC 22
  - SMB
tags:
  - rfc
---
# RFC 22 — SMB

**Status:** draft. [§20](#20.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone changing the SMB adapter — dialects, sessions, opens and
leases, information classes, FSCTLs, the IPC$ services — and anyone deciding
what a Windows client sees when a node goes away.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

SMB (Server Message Block) is the protocol Windows uses for file shares: the
`\\server\share` paths in Explorer. A client signs in, connects to a share,
opens files, and reads, writes and locks through those opens. The server can
call the client back, for instance to take back a caching promise.

This RFC is the SMB adapter: the part of DittoFS that speaks SMB and turns each
request into a call on the filesystem service ([RFC 17](rfc-17-vfs.md)). It
makes every choice the SMB specifications leave to the server — dialects,
credits, how a durable handle is matched on reconnect, how a name made over NFS
looks on Windows — and says which record elsewhere each SMB object stands for.

It owns no durable record of its own — only the opaque `Adapter` bytes it
keeps in RFC 14's records, through the service (§16.4). Names, file IDs and attributes are
[RFC 7](rfc-7-namespace-metadata.md)'s. Opens, leases, locks and durable-handle
identity are [RFC 14](rfc-14-open-state.md)'s. Which shares exist and who may
connect to them are [RFC 17](rfc-17-vfs.md)'s. Behind the service, writes land
in a local journal and move to an object store in the background
([RFC 0](rfc-0-data-lifecycle.md)); SMB sees none of that.

**The problem, with one example.** Take the cluster: gateway nodes **P1**
and **P2** hold client connections, and node **S1** is the primary for
`profiles/alice/` — the one node that orders every change to those files. alice
signs in to **alice-pc**, and Windows attaches her profile container,
`profiles/alice/ODFC_alice.vhdx`: a 30 GB virtual disk with about 10.5 GB used,
held open all day and written in 64 KiB pieces. A connection is fragile; the
disk must not be.

1. **Negotiate and sign in.** alice-pc connects to the cluster's floating
   address, held by P1. The two agree a **dialect** (SMB 3.1.1) and the signing
   and encryption algorithms. alice-pc sets up a **session** with alice's
   Kerberos ticket; the session's keys live in P1's memory only.
2. **Tree connect.** alice-pc connects to `\\dittofs\profiles`. Every request
   is already signed, which the server required at negotiate. The reply carries
   the share's flags: whether every request must also be encrypted, and whether
   the share is continuously available. Assume here that it is not; the end
   of this example says what changes when it is.
3. **Open.** alice-pc sends `CREATE` for the VHDX asking for three things: read
   and write access, letting others read but not write; a **lease** with read,
   write and handle caching (RWH), so it may cache reads, buffer writes and keep
   the file open locally; and a **durable handle**, so the open outlives a lost
   connection. S1 records the open and grants all three. alice-pc gets a
   **FileId** in two halves: one derived from S1's record of the open, valid on
   any node, and one naming an entry in P1's local table.
4. **Network blip.** alice-pc's link drops for 10 s, and its TCP connection with
   it. The open does not go away: it lives at S1, which is still up, and it is
   durable, kept for its timeout — 60 s unless the client asked for more, at
   most 300 s. Every write S1 acknowledged is still in S1's journal.
5. **Reconnect.** alice-pc reconnects its durable open on a new session, and
   the rest of her day — a second sign-in, a node lost, sign-out — is walked in
   [§2](#2.%20A%20profile%20container%2C%20end%20to%20end).

If S1, not the network, had failed, the durable open would have gone with S1's
memory, and with it any write S1 had acknowledged but not yet synced. SMB has no
way to tell the client to resend those. So the reconnect is refused
(`STATUS_OBJECT_NAME_NOT_FOUND`), as on a Windows server that restarted, and the
client sees the disk fail rather than silently lose writes. Only a
**persistent** handle, offered on a continuously available share, survives the
primary: it is stored, every write through it is synced before it is
acknowledged, and it reconnects with its lease. That is why `profiles` is made
continuously available in practice, and why [§2](#2.%20A%20profile%20container%2C%20end%20to%20end)
walks the same day with alice-pc asking for a persistent handle instead.

![Where SMB state lives: alice-pc's connection, session keys and credits, its tree connect to profiles and the volatile half of its FileId are in P1's memory only and rebuilt on reconnect; the second desktop's conflicting CREATE reaches P2, which answers STATUS_PENDING, frees its worker and re-runs the CREATE once the break ends; the open, its deny mode, RWH lease and durable identity, and the names, file IDs, attributes and streams are at S1, the primary of profiles/alice/, which sends alice-pc the lease break](img/rfc22-state-placement.svg)

**The words you need.**

- **gateway node / primary** — the node a client's connection is held by / the
  one node that orders a shard's writes; every node can be both
  ([RFC 0 glossary](rfc-0-data-lifecycle.md#Glossary)).
- **Dialect** — the protocol version a connection agrees at negotiate
  ([§3.1](#3.1%20Dialects)).
- **Session, tree connect** — a signed-in user on one connection / that
  session's attachment to one share; both live in one node's memory and are
  never stored ([§4](#4.%20Sessions), [§5.1](#5.1%20Share%20flags)).
- **Open and FileId** — one open of one file, and the number naming it on the
  wire ([§6.1](#6.1%20Opening%20a%20file),
  [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)).
- **Lease (or oplock)** — a promise that lets a client cache reads (R), buffer
  writes (W) and keep a closed file open (H); taken back by a **break**
  ([§6.2](#6.2%20Oplocks%20are%20leases), [§6.3](#6.3%20Breaks),
  [RFC 14 §5.1](rfc-14-open-state.md#5.1%20What%20a%20grant%20is)).
- **Deny mode** — which other opens a granted open forbids, checked once, at
  open ([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)).
- **Durable / persistent handle** — an open that outlives a lost connection for
  a timeout / one that also outlives the loss of the primary
  ([§6.4](#6.4%20Durable%20and%20persistent%20handles)).
- **Held pending** — a request answered with an interim `STATUS_PENDING` and
  finished later by the adapter, because SMB clients do not retry
  ([§16.5](#16.5%20Errors)).
- **Grace period** — a window after a primary loses state in which only
  reclaims of held state are accepted
  ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)).

**What this RFC promises.**

- Losing a gateway node loses no open state and starts no grace period; the
  client builds a new session and reconnects its durable and persistent opens.
  Opens that were neither are lost, as after any server failure.
- Losing a primary loses its durable opens, which are then refused on
  reconnect; only persistent opens survive it, and no write acknowledged
  through one is lost.
- A durable reconnect succeeds only for the same user, share, client
  identifiers and lease key, within the open's timeout.
- A request waiting on a lease break never holds a worker, and every break ends,
  by acknowledgement or at its deadline, before the waiting request's own
  deadline.
- Every oplock and lease is the same caching grant an NFS delegation is, so SMB
  and NFS clients of one file conflict with each other correctly.
- A name that is not valid UTF-8 is never shown over SMB under a substitute
  spelling; it is hidden.

### Scenarios

**build01 opens a file over NFS that alice-pc keeps handle-cached.**

1. alice-pc holds an SMB open of a file in `profiles` under a lease with handle
   caching, and a deny mode that refuses writers.
2. build01 sends an NFS `OPEN` for writing. Only that deny mode would refuse it,
   so it is not refused yet: build01 is told to retry while alice-pc's handle
   caching is broken ([§6.3](#6.3%20Breaks)).
3. If alice-pc's application had already closed the file and the client was only
   keeping it, the break makes alice-pc close it, and build01's open succeeds.
4. If the open is still in use after the break, its deny mode refuses build01's
   open ([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)).

Outcome: a sharing violation is decided only after the handle caching behind it
is broken, for either protocol.

**alice and bob sign in on one multi-session host.**

1. Both sessions come from one machine with one `ClientGuid`, so they share one
   client record, which holds no principal
   ([§4.4](#4.4%20The%20client%20record%20is%20the%20machine)).
2. After a 10 s blip, bob's session sends a reconnect naming alice's FileId. It
   is refused `STATUS_OBJECT_NAME_NOT_FOUND`: the principal differs
   ([§6.4](#6.4%20Durable%20and%20persistent%20handles)).
3. bob signs out before alice has reconnected. His logoff closes only his own
   session's opens; alice's disconnected open keeps running its timeout
   ([§4.4](#4.4%20The%20client%20record%20is%20the%20machine)).

Outcome: one machine, one record, and each user's opens answer only to that
user.

**A name made over NFS reaches Windows.**

1. On a `posix` share, build01 creates `a:b` over NFS.
2. alice-pc lists the directory and sees the name with the colon shown as
   U+F022, a private-use code point, and can open it
   ([§12.3](#12.3%20Names%20SMB%20cannot%20carry)).
3. build01 creates another name in a legacy 8-bit encoding, not valid UTF-8.
   alice-pc does not see it at all, and cannot open it by name
   ([§12.3](#12.3%20Names%20SMB%20cannot%20carry)).

Outcome: every name is shown under its own spelling, through a reversible
mapping, or not at all.

**How the rest is organised.** §2 walks a profile container through one day —
sign-in, steady state, a network blip, node failures, sign-out — and names the
section holding each rule; read it first. §4 (sessions) and §6 (opens, leases,
breaks, durable handles, replay) carry the story above. §3 sets dialects,
algorithms and sizes; §5 share flags and the `IPC$` services. §7 (byte-range
locks), §8 (change notification), §9 (file information), §10 (control codes),
§11 (security descriptors), §12 (names), §13 (rename and delete) and §15
(Previous Versions) can be skimmed on a first read. §14 covers Witness, the
service that tells a client where to reconnect; §16 gathers every translation
between SMB and the core, with its tests and metrics; §17–§19 the invariants,
conformance checks and other metrics. The appendices hold the reference tables:
create options, information classes, control codes and the character mapping.

---

## In short

- SMB 2.1, 3.0, 3.0.2 and 3.1.1 are served. SMB1 and SMB 2.0.2 are refused.
  Compression, SMB Direct and QUIC are not offered.
- The adapter only translates. Opens, durable and persistent handle identity,
  leases, locks, pending deletes, watches and copy state are
  [RFC 14](rfc-14-open-state.md)'s; names, streams, file ids and attributes
  [RFC 7](rfc-7-namespace-metadata.md)'s; share flags [RFC 16](rfc-16-metadata-store.md)'s
  `ExportPolicy.SMB`; tree connect and enumeration [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees).
  The adapter is built as [RFC 20](rfc-20-adapters.md) describes — its
  lifecycle, request pipeline, codecs and connection handling.
- Sessions, channels, credits and keys live in one gateway node's memory.
  Multichannel binds channels on that node only. After the node is lost the
  client builds a new session and reconnects its durable or persistent opens.
- Writes on a persistent open, and on any open on a continuously available
  share, are synced before they are acknowledged. A durable open does not
  survive the loss of its primary.
- Signing is required for every session at negotiate; encryption is a share
  flag.
- A conflicting request waits on a lease break as an async `STATUS_PENDING`,
  completed by the adapter, never holding a worker.
- Names that are not valid UTF-8 are hidden from SMB; characters Windows cannot
  carry are shown through a fixed private-use mapping. 8.3 names are not
  generated. Timestamp suspension (−1 / −2) is held with the open at the
  primary.

---

## 1. Purpose

MS-SMB2 (the wire protocol) and MS-FSA (how a Windows file system behaves) fix
what a Windows client expects, and leave the server a long list of choices:
which versions and algorithms, how much a client may have in flight, which
metadata queries and control requests exist, how a durable handle is matched on
reconnect, what happens to a name the other protocol made. This document makes
each choice once, says why, and maps each SMB object onto the record that owns
it elsewhere in the set.

It answers:

> **For every choice the SMB specifications leave open, what does this server do,
> and which record in the rest of the set does each wire object stand for?**

### 1.1 Non-goals

This document **MUST NOT**:

- define a record. Opens, durable and persistent identity (CreateGuid,
  AppInstanceId, durable timeout, LockSequence), lease keys, parent lease keys
  and epochs, locks, pending deletes and copy state are
  [RFC 14](rfc-14-open-state.md)'s; numeric file ids
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)) and the absence of 8.3 short names
  ([RFC 7 §3.7](rfc-7-namespace-metadata.md#3.7%20No%20short%20names)) RFC 7's; share policy [RFC 16](rfc-16-metadata-store.md)'s;
- decide admission, tree connect or share enumeration — [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees);
- decide authentication, principals or the mapping of users to Windows
  security identifiers (SIDs) — [RFC 18](rfc-18-identity.md#4.4%20SIDs); or
  what a stored access control list (ACL) means and how it is evaluated —
  [RFC 19](rfc-19-authorization.md#2.%20The%20model). How a Windows security
  descriptor spells that ACL is this RFC's (§11);
- restate MS-SMB2 framing, or the rows of the status table that follow from
  MS-SMB2, which the adapter holds and
  [RFC 20 §4.5](rfc-20-adapters.md#4.5%20One%20error%20table%20per%20protocol) keeps total over
  [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)'s errors;
- answer DFS (Distributed File System) referrals ([RFC 17 §1.1](rfc-17-vfs.md#1.1%20Non-goals)).

## 2. A profile container, end to end

*Non-normative.* The reference workload
([index](rfc-index.md#Reference%20workloads)) keeps each user's Windows profile
in one virtual disk, attached at sign-in and held open, through one handle,
until sign-out. This section walks alice's day on the Start here cluster (P1,
P2, S1 the primary of `profiles/alice/`) and names the section holding each
rule. Assume `profiles` requires encryption and is continuously available.

1. **Sign-in.** alice-pc reaches P1 through the floating address, agrees SMB
   3.1.1 and the first cipher in the server's order that it also offers
   ([§3](#3.%20Dialects%20and%20negotiation)), and signs in with alice's
   Kerberos ticket. The session's keys stay in P1's memory
   ([§4.1](#4.1%20Signing%20in)). Connecting to `\\dittofs\profiles`, it learns
   the share requires encryption and is continuously available
   ([§5.1](#5.1%20Share%20flags)). Because the share is continuously available,
   it also registers with the Witness service on P2 ([§14](#14.%20Witness)).
2. **Attach the disk.** alice-pc opens `ODFC_alice.vhdx` for read and write,
   letting others read but not write, and asks for a read-write-handle (RWH)
   lease and a persistent handle ([§6.1](#6.1%20Opening%20a%20file),
   [§6.2](#6.2%20Oplocks%20are%20leases),
   [§6.4](#6.4%20Durable%20and%20persistent%20handles)). S1 records the open
   and grants both. The FileId it returns works on any node
   ([§6.1](#6.1%20Opening%20a%20file)). In the morning hundreds of users do
   the same at once — the sign-in storm — each also reading a small metadata
   file and releasing it.
3. **Steady state.** For hours, Windows overwrites 64 KiB pieces of the disk in
   random places. Because the open is persistent on a continuously available
   share, every one of those writes is stable: synced to the primary's journal
   before it is acknowledged, whether or not it asked for write-through
   ([§9.4](#9.4%20Writes%2C%20flushes%20and%20stability)). The lease lets
   alice-pc cache reads; nothing breaks it, because nobody else opens the file.
   512 credits keep its pipeline full ([§3.3](#3.3%20Sizes%20and%20credits)).
4. **Network blip.** alice-pc's link drops for 10 s; S1 stays up. The open
   stays at S1 with its lease and deny mode. alice-pc signs in again on P1,
   naming its old session so P1 tears it down first
   ([§4.3](#4.3%20Reconnect%20after%20a%20node%20is%20lost)), connects to the
   share and reconnects its handle; the reconnect is routed by the FileId to S1,
   which checks it is the same user, share, client and lease
   ([§6.4](#6.4%20Durable%20and%20persistent%20handles)). This works for a
   durable open as well as a persistent one: S1 lost nothing. A lock request
   it re-sends because the reply was lost succeeds without being applied twice
   ([§6.5](#6.5%20Replay)).
5. **P1 fails.** The floating address moves to P2, and Witness tells alice-pc
   so ([§14](#14.%20Witness)). Its session is gone with P1, but no open state
   was on P1, so there is no grace period
   ([§4.3](#4.3%20Reconnect%20after%20a%20node%20is%20lost)). alice-pc
   reconnects as in step 4, through P2.
6. **S1 fails.** Another node becomes primary of `profiles/alice/`. The
   persistent open was stored, and every write acknowledged through it was
   synced, so alice-pc reconnects to it with its lease and the disk holds every
   byte it was told was written
   ([§6.4](#6.4%20Durable%20and%20persistent%20handles)). On a share that is
   not continuously available, the handle would have been only durable, held in
   S1's memory with writes S1 had not yet synced: the reconnect is refused
   `STATUS_OBJECT_NAME_NOT_FOUND`, and the profile software sees the disk fail
   instead of reattaching one with a silent gap in it.
7. **A second sign-in.** alice signs in on a second desktop. Its open of the
   disk conflicts with alice-pc's lease: the request waits, without holding a
   worker, while alice-pc is asked to give up handle caching; then alice-pc's
   deny mode refuses it ([§6.3](#6.3%20Breaks)). If instead the profile software
   re-attaches the disk from the new machine with the same app instance id, the
   new open — from another client, by a user who may read the disk — closes
   alice-pc's first
   ([§6.4](#6.4%20Durable%20and%20persistent%20handles)).
8. **Sign-out.** Before letting go, the profile software compacts the disk:
   through the same handle it shrinks the container's end of file, by
   gigabytes at a time (30 GB to 24 GB in one test), with the lease and the
   persistent handle still held ([§9.3](#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)).
   Then alice-pc closes the handle: the open, its lease and its deny mode end
   at the primary ([RFC 14 §3](rfc-14-open-state.md#3.%20One%20table%20per%20file%2C%20at%20one%20primary)), and the session ends on
   its gateway node.

The conformance check for this walk is the profile-container row of
[§18](#18.%20Conformance).

## 3. Dialects and negotiation

A client's first request (`NEGOTIATE`) lists the protocol versions (dialects)
it speaks and, from 3.1.1, the signing and encryption algorithms it supports;
the server picks one of each, and which to offer is its choice. We serve the
SMB 2 and 3 dialects supported Windows releases use, refuse older ones, and
prefer the strongest algorithms.

### 3.1 Dialects

- The server negotiates the highest of **3.1.1, 3.0.2, 3.0, 2.1** the client
  offers.
- **SMB1 is refused.** An SMB1 `NEGOTIATE` naming an SMB2 dialect string is
  answered with the SMB2 multi-protocol response (MS-SMB2 3.3.5.3.1); one
  naming none is answered by closing the connection.
- **SMB 2.0.2 is refused**: a client offering only it is answered
  `STATUS_NOT_SUPPORTED`.
- A 3.0 or 3.0.2 client's check, after signing in, that nobody tampered with
  the negotiation (`FSCTL_VALIDATE_NEGOTIATE_INFO`) is answered.
- **The `NEGOTIATE` response advertises exactly what is served.** A client uses
  a feature only when the response's `Capabilities` names it, so each dialect
  **MUST** set these and no others. "If asked" means set only when the
  client's `NEGOTIATE` request sets the same bit, as MS-SMB2 3.3.5.4 sets
  them:

  | Capability | 2.1 | 3.0, 3.0.2 | 3.1.1 |
  | --- | --- | --- | --- |
  | `SMB2_GLOBAL_CAP_LEASING` (§6.2) | set | set | set |
  | `SMB2_GLOBAL_CAP_LARGE_MTU` (§3.3) | set | set | set |
  | `SMB2_GLOBAL_CAP_MULTI_CHANNEL` (§4.2) | — | if asked | if asked |
  | `SMB2_GLOBAL_CAP_PERSISTENT_HANDLES` (§6.4) | — | if asked | if asked |
  | `SMB2_GLOBAL_CAP_ENCRYPTION` (§5.1) | — | if asked | clear: 3.1.1 negotiates ciphers by context (§3.2) |
  | `SMB2_GLOBAL_CAP_DIRECTORY_LEASING` (§6.2) | clear | clear | clear |
  | `SMB2_GLOBAL_CAP_DFS` | clear | clear | clear |
  | `SMB2_GLOBAL_CAP_NOTIFICATIONS` | — | — | clear |

  Without `PERSISTENT_HANDLES`, a client on a continuously available share
  asks only for durable handles, which a failover refuses (§6.4); without
  `LEASING` or `LARGE_MTU`, it falls back to oplocks or 64 KiB requests.

> decision: old dialects are refused. SMB1 has no unbroken signing, no durable
> handles and no leases; SMB 2.0.2 has no leases and no client identifier
> (`ClientGuid`), which is how [RFC 14](rfc-14-open-state.md#2.1%20Client) knows a client.
> Only clients older than any supported Windows release lose out: Windows Vista
> and Windows Server 2008 (not R2), whose highest dialect is 2.0.2, cannot
> connect; Windows 7 and Server 2008 R2 negotiate 2.1. Revisit 2.0.2 only for a
> client population that cannot negotiate 2.1.

### 3.2 Algorithms, and what is not offered

On 3.1.1 the client offers algorithms in negotiate contexts, and the server
takes the first of its own order that the client also offers:

- protecting the negotiation (preauthentication integrity): SHA-512, the only
  one defined;
- encryption: AES-256-GCM, AES-128-GCM, AES-256-CCM, AES-128-CCM;
- signing: AES-GMAC, AES-CMAC; HMAC-SHA256 on 2.1;
- the server name the client meant (netname context): read, not checked;
- **not offered**: compression, the RDMA transform, and the transport
  capabilities context that signals QUIC.

A 3.1.1 client that offers no cipher the server shares gets no encryption, and
cannot reach a share that requires it ([§5.1](#5.1%20Share%20flags)).

> decision: compression is not offered. Content is already compressed below
> the protocol where that pays ([RFC 5](rfc-5-transforms.md)); per-message
> compression would cost the gateway node CPU on every read. Offer it when a
> wide-area deployment shows reads limited by link bandwidth, not by the node.

> decision: SMB Direct (over RDMA, remote direct memory access) and SMB over
> QUIC (a UDP transport for use across the internet) are not offered: the
> adapter has neither transport and TCP clients need neither. Offer SMB Direct
> with NFS over RDMA when a deployment has RDMA networks; QUIC when clients must
> reach shares across the internet without a VPN.

### 3.3 Sizes and credits

Credits are SMB's flow control: each request spends credits and each response
grants some back, so the credits a client holds bound how much it can have in
flight.

- The largest read, write and control request (`MaxReadSize`, `MaxWriteSize`,
  `MaxTransactSize`) is **8 MiB**, a request costing one credit per 64 KiB.
- Each connection targets **512** credits outstanding. A synchronous response,
  and the interim response of a request held pending, grants what the client
  asks, at least 1, up to the target; the final response of a request answered
  asynchronously grants 0, since its interim response already granted them
  (below). The server never grants past the target and never revokes credits
  already granted.
- Sequence windows, credits and outstanding requests are per connection, in the
  memory of the gateway node.
- **A held request's credits are granted in its interim response.** A request
  answered `STATUS_PENDING` (§6.3, §16.5) is granted its credits in the interim
  response (MS-SMB2 3.3.4.2), not held back until the final one, so the
  client keeps sending on its other trees while it waits.
- **Held requests draw from the node, with a floor per connection.** Credits
  bound what is in flight, not what is held: a request held pending has
  already been granted its credits back, and holds last up to
  `smb_pending_cap`. Each connection is guaranteed **256** held requests and
  **32 MiB** of held `WRITE` data — the credit target's worth, 512 credits of
  64 KiB — and past either it draws on the node's one hold pool, a sixteenth
  of the process's memory limit, in which each held request is charged its
  write's bytes and at least 64 KiB
  ([RFC 24 §4.3](rfc-24-resources.md#4.3%20Held%20requests%3A%20where%20their%20bytes%20go)
  owns the pool and which pool holds a held request's bytes). The count is the
  adapter's ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)): it counts
  every request of the connection it holds, whether between re-drives or
  waiting inside the service behind a clone under `vfs.WithHold`
  ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)), from its first hold
  until it is answered. A request the adapter would hold with its connection
  past a floor while the pool cannot take it is not held, and is answered
  `STATUS_INSUFFICIENT_RESOURCES` at once (§16.5), which MS-SMB2
  3.3.4.2 allows when asynchronous processing would exceed the server's
  resources, and nothing is held for it. Waiting locks (§7) are not counted:
  they hold no data and no worker, and end only as RFC 14's waiter does.

> ponytail: one credit target and one hold floor per connection, neither a
> setting, over RFC 17's one hold pool per node. 512 credits keep a client's
> pipeline full on a LAN. A multi-session desktop host carries every user's
> sessions on one connection, so 40 users' profile disks at queue depth 8 pass
> 256 held requests within a second of a freeze or a store stall; the pool,
> 4 GiB on a 64 GiB node, holds those 320 requests, and the floor keeps one
> busy host from starving the others when the pool is spent. The ceilings:
> many idle connections each allowed 512 credits, and a node whose pool is
> spent refuses past-floor holds with `STATUS_INSUFFICIENT_RESOURCES`, which
> guests see as failed I/O. Adapt the credit target to node memory when idle
> connections exhaust a node; the pool's own ceiling and trigger are RFC 24
> §4.3's, read from `adapter_hold_refusals_total{protocol="smb"}` (§19).

## 4. Sessions

A session is a user signed in on a connection; it holds the keys that sign and
encrypt every later message. The standard lets one session span several
connections (multichannel). We keep each session, with all its connections, in
one gateway node's memory; a client that loses the node signs in again.

### 4.1 Signing in

- `SESSION_SETUP` carries SPNEGO (the negotiation wrapper that picks an
  authentication method) with Kerberos or NTLMv2 (NT LAN Manager version 2).
  NTLMv1 and LM (LAN Manager) are refused. Which identity results, guest and anonymous
  access, and whether NTLM is allowed at all are
  [RFC 18 §3](rfc-18-identity.md#3.%20Authentication)'s.
- A Kerberos ticket may list the client addresses it is valid from. Only its
  IPv4 and IPv6 entries restrict which client may use it: a ticket with none
  is accepted from any address, and a ticket with any **MUST** be refused
  unless the connection's address is one of them. Entries of other types,
  such as NetBIOS names, are ignored.
- The session key, signing key, encryption and decryption keys and the
  preauthentication hash are held in the memory of the gateway node, and
  **MUST NOT** be stored or sent to another node.
- **Signing is required server-wide, at negotiate.** Every `NEGOTIATE`
  response **MUST** set `SMB2_NEGOTIATE_SIGNING_REQUIRED`, so every client
  signs from session setup on. After a session is set up, every request on it
  **MUST** be signed or encrypted, and one that is neither is
  `STATUS_ACCESS_DENIED`. A request whose signature fails verification is
  answered `STATUS_ACCESS_DENIED` on its message ID too, and the connection
  stays open (MS-SMB2 3.3.5.2.4): it is never dropped unanswered, which would
  leave the client waiting out its timeout. There is no per-share signing setting: SMB2 cannot
  tell a client that one share needs signing and another does not, so a
  per-share requirement could only refuse an unsigned client's every request on
  that share.
- A share's encryption flag ([§5.1](#5.1%20Share%20flags)) is enforced per
  share connection, not per session: a session that does not encrypt may
  connect to an encrypting share, and every request on that tree is then
  encrypted. Only a client that cannot encrypt at all is refused at tree
  connect.

- **A session without a key reaches only what admits it.** An anonymous (null)
  session and a guest one are both set up as anonymous
  ([RFC 18 §3.6](rfc-18-identity.md#3.6%20Anonymous%20and%20guest%20sessions)),
  hold no session key and cannot sign or encrypt, so they are exempt from the
  signing requirement (MS-SMB2 3.3.5.5.3). That exemption reaches only as far
  as this rule lets them:
  - a **null** session's `TREE_CONNECT` to any share but `IPC$` **MUST** be
    `STATUS_ACCESS_DENIED`, always;
  - a **guest** session's `TREE_CONNECT` **MUST** be `STATUS_ACCESS_DENIED`
    unless the share's policy enables guest access (§5.1), which is off by
    default, and the share does not encrypt: a share that requires encryption
    never admits a guest, whatever its guest flag;
  - on `IPC$` either reaches only §5.2's services.

  A session setup whose credential fails is never made anonymous or guest; it
  fails. These rules gate `TREE_CONNECT`; a session re-authenticated as null or
  guest keeps the trees and keys it already has (re-authentication, below).
- **Re-authentication in place.** A `SESSION_SETUP` naming an existing
  `SessionId` without `SMB2_SESSION_FLAG_BINDING` re-authenticates that session
  ([RFC 18 §3.8](rfc-18-identity.md#3.8%20Re-authentication)): the adapter
  passes the session's current evidence in `SignInStep`, and a success keeps the
  session's trees, opens and keys and replaces the session's identity with the
  one the new proof establishes, in one step, whoever it names, null and
  guest included. Every open
  handle keeps the access granted at its open. Any failed re-authentication
  tears the session down as
  MS-SMB2 3.3.5.5.3 requires: its opens close, pending change notifications
  complete `STATUS_NOTIFY_CLEANUP`, and its trees are removed.

> decision: a successful re-authentication replaces the session's identity in
> place, and open handles keep the access granted at open. MS-SMB2 3.3.5.5 and
> the reference servers behave this way, and the reference suite's
> `smb2.session.reauth2`–`reauth4` depend on it. The ceiling: a handle opened by
> the earlier identity keeps that identity's grant, whoever the session now
> acts as; every new open and path call is decided for the new identity. A
> session re-authenticated as null or guest keeps its trees and the keys its
> first sign-in derived, so it is never keyless, and a new `TREE_CONNECT` from
> it follows the null and guest rules above. Overturned by a client shown to
> rely on a re-authentication narrowing an existing handle's access.
- **An expired session keeps its release commands.** On a session past its
  evidence's expiry, every command but `SESSION_SETUP` (re-authentication),
  `LOGOFF`, `CLOSE`, an unlocking `LOCK` and an oplock or lease break
  acknowledgement is refused `STATUS_NETWORK_SESSION_EXPIRED`, so a client can
  release what it holds while it renews its ticket.
- **The NTLM MIC is identity's to check**
  ([RFC 18 §3.5](rfc-18-identity.md#3.5%20NTLMv2)), so the adapter keeps the
  `NEGOTIATE`, `CHALLENGE` and `AUTHENTICATE` bytes exactly as they crossed
  the wire and passes them in `SignInStep`.

> decision: a keyless session is exempt from signing rather than refused
> outright, and the exemption's ceiling is the shares that admit it: `IPC$`
> for both kinds, and for a guest only a share whose guest flag is set and that
> does not encrypt. A null session reaches `IPC$` alone because it is worth only
> a share list and name lookups, which carry no file data, and pre-sign-in tools
> that browse shares keep working. A guest reaches data only where an operator
> chose to publish it unsigned, as with `guest ok` on a Samba share; Windows
> Server and the common NAS products ship with guest access off, as this
> default is. An encrypting share is excluded because its promise is that no
> byte travels in clear, and a keyless session cannot keep it. The confinement
> is this RFC's, not RFC 18's, so no admission rule elsewhere can widen it.
> Refuse keyless sessions outright if §5.2's services are shown to leak what an
> unauthenticated caller must not see; withdraw the guest flag if guest shares
> are shown to be reached by sessions that meant to sign.

### 4.2 Multichannel only within one node

Multichannel lets one session use several TCP connections, say one per network
card. A client asks for the server's addresses
(`FSCTL_QUERY_NETWORK_INTERFACE_INFO`) and binds new connections to its session.

- A session's channels **MUST** all be on one gateway node.
- The address query lists only the addresses the answering node holds now, so
  a client never discovers another node's.
- A bind (`SESSION_SETUP` with `SMB2_SESSION_FLAG_BINDING`) for a session this
  node does not hold is answered `STATUS_USER_SESSION_DELETED` (MS-SMB2
  3.3.5.5.2); the client keeps the channels it has.

> decision: no multichannel across nodes. A session's keys, credits, open table
> and replay state live on the node that set it up
> ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees));
> sharing them would make every request a lookup on another node. Channels to
> one node still give the bandwidth; what is lost is riding out that node's loss
> without a reconnect. Revisit if one node's network cards cap a single client
> below what a benchmark shows clients need.

### 4.3 Reconnect after a node is lost

- When a gateway node is lost, its sessions, tree connects and the volatile
  halves of FileIds go with it
  ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)).
- The client reaches a surviving node through a floating address or Witness
  ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)), sets up a new
  session, tree connects, and reconnects its durable and persistent opens
  ([§6.4](#6.4%20Durable%20and%20persistent%20handles)).
- **`PreviousSessionId` is honoured on the node that holds it.** A
  `SESSION_SETUP` naming a previous session that this node still holds **MUST**
  tear that session down before it completes, as MS-SMB2 3.3.5.5.3 requires,
  but only once this setup's `SignIn` has succeeded and bound to the same
  `PrincipalID` as the previous session's. A failed, anonymous or other-principal
  setup leaves the previous session untouched, and the field is then ignored,
  never answered differently, since session IDs are predictable. On teardown that session's volatile opens close and its own
  durable and persistent opens are marked disconnected at their primaries, so
  the reconnects that follow find them free. Opens of other sessions under the
  same `ClientGuid` (§4.4) are untouched.
  This is the common case after a
  network blip, where the client comes back to the same node through the same
  floating address before the node has noticed the old connection is dead. A
  previous session this node does not hold — its node was lost — names nothing
  and is ignored.
- **A disconnect is per session, at its last channel.** Each open records the
  session that made it or last reconnected it, named by the gateway node,
  the node epoch the session was created under and the session ID together,
  since a session ID is unique only on its node and only within one process:
  a node that restarts takes a new node epoch, so a session it sets up after
  the restart never names a persistent open whose session died with the old
  process, though both carry the same session ID. Session IDs count from 1 in
  each process
  ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens),
  [RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)),
  and the adapter calls the service's `Disconnect` for a session
  ([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)) only when the last of its
  channels ends (MS-SMB2 3.3.7.1): that session's volatile opens close and its
  durable and persistent opens start their timeouts. Losing one channel of a
  multichannel session ends nothing while another channel lives, and one
  user's disconnect on a multi-session host starts no timeout on another
  user's opens. **(cluster)** When the gateway node holding a session is
  lost, no adapter is left to call `Disconnect`: the primary treats every open
  whose session belongs to a node whose node lease lapsed as disconnected at
  the lapse, and its `Timeout` starts then
  ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).
- Volatile opens are lost, as after any server failure.
- A lost gateway node loses no open state, so no grace period runs.
- A lost primary is a failover. Its persistent opens are stored and reconnect;
  its durable opens are gone, and their reconnects are refused
  ([§6.4](#6.4%20Durable%20and%20persistent%20handles)). The primary signals
  the loss by having the connections of every client whose record names the
  shard broken, so each client reconnects and reclaims its persistent opens
  ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)); its
  durable opens in other shards survive that as a disconnect. New opens wait out
  the shard's grace
  ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)), held
  pending until it ends ([§16.5](#16.5%20Errors)).

### 4.4 The client record is the machine

SMB names a client by its `ClientGuid`, and one Windows machine sends one
`ClientGuid` on every connection, including a multi-session desktop host that
carries many users' sessions at once.

- The client record of an SMB client **MUST** be keyed by `ClientGuid` alone
  and hold no principal
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)): a negotiate naming a
  known `ClientGuid` finds that record whoever then authenticates, and is never
  refused for its principal.
- Principals bind where SMB binds them: each session to the user who set it
  up, and each open to its opener
  ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)). A reconnect, a replay and
  every call through a FileId are checked against the open's principal, never
  the record's.
- What RFC 14 keeps per client is therefore per machine: the grant budget, and
  the caching fence after a missed break (§6.3).
- **A logoff closes only its own session's opens** (MS-SMB2 3.3.5.6), and
  releases nothing else of the record: another user's disconnected durable or
  persistent opens on the same machine keep running their timeouts. The record
  itself goes only when it holds no open and no durable open's timeout is still
  running ([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)).
  Otherwise, on a multi-session host after a network drop, one user signing out
  before another reconnects would close the second user's profile container.

> decision: one record per machine, with principals per session and open,
> because that is the identity SMB carries. A record per (`ClientGuid`,
> principal) would let a second user's negotiate split one machine's
> connections across two records, and a principal on the record would refuse
> every user but the first on a shared host. The cost: a missed break fences
> caching for every user of that machine for 90 s, and the grant budget is
> shared by them. Revisit if multi-session hosts are seen held at the budget.

## 5. Shares and IPC$

A tree connect attaches a session to one share, such as `\\dittofs\profiles`;
its reply tells the client whether requests must be encrypted and whether the
share is continuously available (CA: handles survive a server's
loss). `IPC$` is not a share but the adapter's RPC endpoint, reaching no file,
through which Windows calls management services — listing shares, turning a security identifier into a name — as
remote procedure calls (DCE/RPC) over named pipes. Admission is
[RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s.

### 5.1 Share flags

`TREE_CONNECT` is `Root` with the share's name ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees));
a tree connect to `IPC$` makes no service call.
Its response carries the share's `ExportPolicy.SMB`:

- **Encrypt.** The share is marked as encrypting data
  (`SMB2_SHAREFLAG_ENCRYPT_DATA`). A session that does not itself encrypt
  **MAY** connect to it. The tree connect reply that carries the flag **MUST**
  go unencrypted unless the session encrypts, since the client learns from that
  reply that it must encrypt; every later request on the tree **MUST** be
  encrypted, and an unencrypted one is `STATUS_ACCESS_DENIED`. Only a client
  that cannot encrypt at all — a 2.1 client, a 3.0 or 3.0.2 client that does
  not advertise encryption (`SMB2_GLOBAL_CAP_ENCRYPTION`), or a 3.1.1 client
  that shares no cipher with the server ([§3.2](#3.2%20Algorithms%2C%20and%20what%20is%20not%20offered))
  — is refused at tree connect, with `STATUS_ACCESS_DENIED`.

  > decision: encryption is enforced per tree, not per session, as Windows
  > servers do (MS-SMB2 3.3.5.7). Refusing an unencrypted session at tree
  > connect would refuse every client that signs in without session
  > encryption and expects the share flag to switch it on, which is how
  > Windows clients reach an encrypting share. Nothing travels in clear but
  > the tree connect reply itself, which carries no file data. Revisit if a
  > deployment must refuse any session that does not encrypt from its first
  > message; that is a session-level setting, not this share flag.
- **Guest.** A guest session may connect to the share (§4.1); off by
  default, and of no effect on a share that encrypts. Nothing on the wire.
- **Continuously available.** The share is marked continuously available and
  clustered (`SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY`,
  `SMB2_SHARE_CAP_CLUSTER`, 3.0 and later); persistent handles are granted
  ([§6.4](#6.4%20Durable%20and%20persistent%20handles)); Witness is served
  ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)).
- **Hidden.** Nothing on the wire; the share is omitted from enumeration
  ([§5.2](#5.2%20IPC%24%20and%20the%20RPC%20services)).
- **Access-based enumeration.** The share answers `TREE_CONNECT` with
  `SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM`, and every `QUERY_DIRECTORY` on
  it returns only the entries
  [RFC 19 §3.7](rfc-19-authorization.md#3.7%20Listings%20hide%20what%20the%20caller%20cannot%20read)
  lets through. The filter is the service's listing, never the adapter's.
- The caller's maximal access (`MaximalAccess`) is the service's `Access` on
  the share root.

The flags every share reports whatever its policy are listed in
[Appendix C](#Appendix%20C%20%E2%80%94%20flags%20every%20share%20reports).

### 5.2 IPC$ and the RPC services

`IPC$` is the adapter's own, reaches no file, and serves named pipes for exactly
three services:

| Service | Calls | Answered from |
| --- | --- | --- |
| server service (srvsvc) | `NetShareEnumAll`, `NetShareGetInfo` (levels 0, 1, 2, 501, 1005), `NetSrvGetInfo` | `ListShares`, filtered as [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees) says, and for `NetShareGetInfo` by the name asked; `NetSrvGetInfo` the installation's name |
| workstation service (wkssvc) | `NetWkstaGetInfo` (100, 101) | the installation's name |
| local security authority (lsarpc) | `OpenPolicy2`, `LookupSids2/3`, `LookupNames3/4`, `Close` | RFC 18's principals, spelled as [RFC 18 §4.6](rfc-18-identity.md#4.6%20Names) says — the security tab cannot show names without it |

Every other call is answered `DCERPC_FAULT_OP_RNG_ERROR`. Witness is served over
RPC on TCP, not on `IPC$` ([§14](#14.%20Witness)).

> ponytail: three services, the subset Explorer, `net view` and the security tab
> call. Any other management tool that needs a fourth fails. Add a service when
> a supported client workflow fails without it, never for completeness.

## 6. Opens and leases

`CREATE` opens (or creates) a file. It names the access wanted, what other
opens may do meanwhile (share access, [RFC 14](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)'s deny
mode), and optionally a lease to cache under and a durable handle that outlives
a lost connection. All of it lives at the file's primary as RFC 14 records. The
server chooses which caching to grant, how a client waits while another gives
up its cache, and how a reconnect finds its open.

### 6.1 Opening a file

- `CREATE` is one `Open` ([RFC 17 §4.1](rfc-17-vfs.md#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection)):
  disposition, desired access, share access as the deny mode, a wanted lease and
  a durability, in one atomic step.
- Each create option and create context (a tagged extra in the request: a lease
  request, a durability request, a snapshot time) maps to one service concept or
  is refused, as [Appendix D](#Appendix%20D%20%E2%80%94%20create%20options%20and%20contexts)
  lists. Three are worth knowing: opening by numeric id is refused, because
  nothing indexes files by id ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)); a create carrying an app instance id closes an
  earlier open with the same id, from another client and only for a caller
  that may read the file ([§6.4](#6.4%20Durable%20and%20persistent%20handles));
  virtual-disk sharing is refused.
- The FileId's persistent half **MUST** be derived from the RFC 14 open's
  `OpenID`, unique across the installation
  ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), and name the open's shard, so any node finds the open from it and routes a
  reconnect to its primary without the path (§6.4).
- Its volatile half names the entry in the gateway node's table and is
  invalid on any other node.
- **Generic rights are mapped before the open.** `GENERIC_READ`,
  `GENERIC_WRITE`, `GENERIC_EXECUTE` and `GENERIC_ALL` in `DesiredAccess`
  (MS-SMB2 2.2.13.1) **MUST** be mapped through the file generic mapping to
  `FILE_GENERIC_READ`, `FILE_GENERIC_WRITE`, `FILE_GENERIC_EXECUTE` and
  `FILE_ALL_ACCESS` (MS-FSA 2.1.5.1.2) before `Open`, and so are the same
  bits in every access mask of a descriptor a client sets (§11), before the
  call. Directories use the same bits. This RFC is the one owner of the mapping:
  the core receives entry rights only, and refuses a generic bit that reaches it
  ([RFC 19 §2.4](rfc-19-authorization.md#2.4%20Rights)). Unmapped, no stored entry and no mode bit grants them, and a Linux
  client, which sends `GENERIC_READ` and `GENERIC_WRITE` on its opens, is
  refused every open.
- **The mapped sets' minor rights follow the mode's bits.** The mapped sets
  also ask for `SYNCHRONIZE`, `READ_CONTROL`, `FILE_READ_EA`,
  `FILE_WRITE_EA`, `FILE_READ_ATTRIBUTES` and `FILE_WRITE_ATTRIBUTES`. On a
  mode-governed file they are granted by the ACL the mode implies
  ([RFC 19 §4.1](rfc-19-authorization.md#4.1%20The%20ACL%20a%20mode%20implies)),
  which owns the rule; on the wire it means `SYNCHRONIZE`, `READ_CONTROL`,
  `FILE_READ_EA` and `FILE_READ_ATTRIBUTES` go with any mode bit the caller
  holds, read, write or execute, and `FILE_WRITE_EA` and
  `FILE_WRITE_ATTRIBUTES` with write. A request is granted or refused for every
  right it asks, as MS-DTYP's access check decides, so a `GENERIC_WRITE` open
  by a group member holding write only is granted `READ_CONTROL` and
  `FILE_WRITE_ATTRIBUTES` rather than refused for them. Without this, a group
  member's `mount.cifs` open of a 0664 file is refused, and so is a
  write-only open of a 0620 file.
- **Times set through an open follow its grant.** A `SET_INFO` of
  `FileBasicInformation` times through an open whose grant holds
  `FILE_WRITE_ATTRIBUTES` — granted by a mode bit included — stores the times
  the client gives, as a Windows server does: `FILE_BASIC_INFORMATION` has no
  "server time" value, so a rule that let a non-owner set only the current
  time could not be expressed over SMB, and every Explorer or `robocopy` copy
  onto a team share would fail to set its times. The owner rule still binds
  everything else such a set may change, the DOS attributes included, which a
  non-owner without an ACL grant is refused `ErrPerm`,
  `STATUS_ACCESS_DENIED`, and it binds NFS `SETATTR` of times unchanged
  ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)).
- **`DELETE` at open consults the parent.** SMB deletes and renames by
  opening with `DELETE`, and a file's own mode grants no delete. An open
  asking for `DELETE` **MUST** be granted it when the file's own ACL or mode
  grants it or when the directory the open resolved through grants the caller
  "delete a child" (`FILE_DELETE_CHILD`, or on a mode-governed directory
  write and execute, with the sticky rule), and `FILE_READ_ATTRIBUTES` when
  the file grants it or that directory grants listing (MS-FSA 2.1.5.1.2.1). The
  adapter passes the directory the path resolved through, and the name, in
  the `OpenRequest`
  ([RFC 17 §4.6](rfc-17-vfs.md#4.6%20One%20chokepoint)), and the chokepoint
  decides
  ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)).
  Without it, in a 0775 directory a group member can delete over NFS a file
  she cannot delete or rename from Explorer.

### 6.2 Oplocks are leases

An oplock (opportunistic lock) is SMB's older caching promise, held by one open;
a lease is the newer one, held under a key the client chooses so several of its
opens share it. Both let a client cache reads, buffer writes or keep a closed
file open locally, until the server takes the promise back.

- Every oplock is an RFC 14 caching grant, as a lease is, and so is the same
  kind of grant as an NFS delegation: Level II is read, exclusive is read and
  write, batch is read, write and handle.
- An oplock's grant is keyed by its open, so two opens by one client break each
  other as oplocks always have; a lease's grant is keyed by its lease key, so
  they do not.
- The lease key, parent lease key and lease epoch are carried on the grant
  (`Key` and `Epoch`, the parent lease key in the grant's `Adapter`,
  [RFC 14 §2.4](rfc-14-open-state.md#2.4%20CachingGrant)). One grant covers
  every open of a file by one client under one key; a key already naming a
  grant on another file is refused. A v2 lease's `Epoch` rises with every
  change of its caching, and the new value is sent with the change.
- Directory leases are not offered: `SMB2_GLOBAL_CAP_DIRECTORY_LEASING` is
  clear, and a lease request on a directory is granted none.

> decision: no directory leases, for the same reason NFS has no directory
> delegations ([RFC 21 §5.3](rfc-21-nfs.md#5.3%20Delegations)): every create in
> the directory would break one. The cost is a round trip each time Explorer
> re-lists a folder. Revisit when a measured browse of a large share shows
> listing round trips (`QUERY_DIRECTORY`) dominating, together with RFC 14's
> open question 4.

### 6.3 Breaks

When another client's request conflicts with a lease, the server takes the
lease back (a break): the holder flushes what it buffered and acknowledges,
while the request waits. The service reports the conflict as `ErrDelay`
([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)); NFS clients retry on that, SMB
clients do not, so the adapter finishes the request itself:

1. answers an interim `STATUS_PENDING` with an async ID, and frees the worker;
2. lets the primary send the holder a break (`LEASE_BREAK_NOTIFICATION`, or
   `OPLOCK_BREAK`) through its callbacks, requiring an acknowledgement
   (`ACK_REQUIRED`) when write or handle caching is broken;
3. re-runs the request when the holder acknowledges, its break reaches the
   deadline ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)),
   or the client sends `CANCEL`, and completes the async response with the
   result.

The hold and the re-run are the pipeline mechanism of
[RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests), not handler code.

![A lease break on a timeline: at 0 s the second desktop's CREATE reaches P2, which answers an interim STATUS_PENDING with an async ID and frees its worker; S1 sends alice-pc a break with ACK_REQUIRED; P2 re-runs the CREATE on a backoff of 10 ms doubling to 1 s; when alice-pc acknowledges, the re-run meets alice-pc's deny mode and the async response is STATUS_SHARING_VIOLATION; without an acknowledgement, at the 35 s break deadline the lease is set to none, the opens stay, the machine is fenced from caching for 90 s, and the request is re-run; the held CREATE's own deadline is smb_pending_deadline, 60 s by default](img/rfc22-lease-break.svg)

- A break from read to none needs no acknowledgement.
- **Handle caching is broken before a sharing violation.** A `CREATE` that
  would fail on the deny mode of another client's open, where that open is
  covered by a grant with handle caching, **MUST NOT** fail at once: the
  holder's handle caching is broken first, and the deny modes are checked
  again once the break ends (MS-FSA 2.1.5.1.2). A handle-cached open may be
  one its application has already closed and the client is only keeping; the
  break makes the client close it, and the create then succeeds. Only an open
  still in use after the break refuses the create.
- **The same holds for a request from another protocol.** An NFS `OPEN`, or an
  anonymous or delegation-stateid read or write, that would be refused only by
  the deny mode of a handle-cached SMB open waits for the handle-caching break
  in the same way, as NFS's retry
  ([RFC 21 §5.3](rfc-21-nfs.md#5.3%20Delegations)); a share-mode refusal of
  any request is decided only after the handle caching covering the
  conflicting open is broken
  ([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)).
- A lock request from another client recalls, to none, a grant with read or
  write caching on the file before the lock is decided (MS-FSA 2.1.5.7;
  [RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols)), as
  an NFS lock does ([RFC 21 §5.3](rfc-21-nfs.md#5.3%20Delegations)). An SMB
  lock request that may wait is left waiting meanwhile (`ErrBlocked`, §7); one
  asked to fail at once is held pending until the recall ends (`ErrDelay`),
  never refused for it
  ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)).
- **The break deadline is 35 s**, the value Windows servers use (MS-SMB2
  leaves it to the implementation), and is RFC 14's recall
  deadline for SMB grants
  ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
- **A request held pending outlives the break.** The adapter gives every
  request it holds pending for `ErrDelay` on a break, recall or clone — a
  `CREATE` waiting on a break — a deadline of `smb_pending_deadline` from its
  arrival ([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings);
  60 s by default), set by the adapter as the call's deadline, the longest
  [RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks) allows a request held
  pending. The setting must exceed the break deadline, so a
  waiting `CREATE` always sees the break end and is re-run, rather than failing
  at RFC 17's 30 s default while the holder still has 5 s to answer.
- **A request refused for grace outlives grace.** A `CREATE` or lock refused
  `ErrGrace` is held pending until the shard's grace ends plus the 35 s break
  deadline, measured from its arrival
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks),
  [RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)),
  and re-run when grace ends. A failover's or restart's grace lasts one lease
  period, 90 s, and ends earlier only when every client has finished
  reclaiming, so
  `smb_pending_deadline` would fail a sign-in made just after a failover while grace still
  refused it, and the re-run, which may meet a break, needs the 35 s after.
  **SMB sees no grace on a namespace move.** The grace the new owner holds
  moved shares in is NFS-only: it refuses NLM and NFSv4 requests, and no SMB
  `CREATE`, lease, deny mode or lock on those shares meets it, since a carried
  persistent open keeps its own per-client timer
  ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step),
  [RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)).
- **A request refused for a frozen share outlives the freeze.** A request
  answered `ErrDelay` because its share is quiesced or frozen for a move is
  held pending until the freeze ends plus 35 s, and never longer than
  `migration.freeze_timeout` plus 35 s
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks),
  [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)),
  then re-run. A freeze lasts up to `migration.freeze_timeout`, 5 minutes by
  default, so `smb_pending_deadline` would answer a write held by a two-minute
  freeze as a failure. If the share's namespace
  has moved when the freeze ends (`DelayFrozen` with `Moved` set, then
  `Revoked` with `RevokedShare`), what the client sees depends on the address its
  connection arrived on. **On an address the move names**, which the moved
  revocation carries ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)), A stops
  serving entirely from its release: it closes every connection on that
  address, with no status for held requests, and accepts no new connection
  there, so the client reconnects through the address once it reaches the
  new owner
  ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)).
  **On any other address**, the adapter ends only the moved shares' trees, as
  a share's removal does (§16.5): requests on them, the held one and a waiting
  lock included, are answered `STATUS_NETWORK_NAME_DELETED`, an outstanding
  `CHANGE_NOTIFY` `STATUS_NOTIFY_CLEANUP`, and the client's other trees on the
  connection go on. **A continuously available share's trees end by their
  connection instead**, on any address: the adapter closes every connection that carries a
  tree of a moved continuously available share, without answering its held
  requests, because a client enters its persistent-handle reconnect loop on a
  lost transport, not on `STATUS_NETWORK_NAME_DELETED`, which reaches the
  application as a failed handle. The connection's trees on other shares
  reconnect with it
  ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step),
  to be confirmed by its `S-snap-move-ca-windows` row and §18's).
  Durable handles are refused at the new owner and
  reopened, as after a failover; only persistent opens, whose records the move
  carries, reconnect there
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks),
  [RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).
  **A moved persistent open's timeout starts at its own client's first tree
  connect to its share at the new owner** — a tree connect from the open's
  `ClientGuid`, not any client's — not when the new owner begins
  serving, so one client's repoint starts no other client's timers, and the
  open is kept for at most `migration.reconnect_wait`
  ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step))
  from that serving if no tree connect comes. The client arrives only after
  the operator repoints the name and its DNS cache expires, which easily
  passes the open's 60 s.
  The client reaches the new owner only through the name it used: the
  operator moves that name's address to the new installation, or points the
  name there, once the new owner serves (its claim `owned`,
  [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)),
  and Witness clients are sent
  `RESOURCE_CHANGE` for it (§14). This server sends no referral (§1.1).
- **A write refused for capacity is held, not failed, while the cause is
  transient.** A write answered `ErrDelay` because the journal's capacity or
  the share's journal limit refuses it while offload or repack can drain it —
  a remote outage included — is held pending up to `smb_pending_cap`
  ([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings);
  10 minutes by default, longer than a freeze plus 35 s) and re-run; only at
  that cap is it answered `STATUS_DISK_FULL`
  ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)).
  The service's deadline ends each re-run, never the cause's class: a
  transient refusal is never turned into `ErrNoSpace` because a deadline ran
  out. A permanent cause is `STATUS_DISK_FULL` at once (§16.5).
- **A waiting lock has no deadline** (§7): it holds no worker and ends only as
  RFC 14's waiter does.
- **A break not acknowledged by its deadline downgrades; it does not close.**
  At the deadline the lease or oplock is set to none (MS-SMB2 3.3.6.1,
  3.3.6.5), and the waiting request is re-run. The holder's opens stay open: its deny modes and locks still
  apply, and its later reads and writes through them are served as ordinary
  I/O on an open that holds no caching. The client — the machine (§4.4) — is
  fenced from caching: it is offered no new lease or oplock for 90 s after the
  timed-out break at that primary, each further timed-out break restarting the
  period; its other leases and oplocks end by their own breaks
  ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
  An SMB client has no lease, so a fence tied to one would never lift while its
  record exists.
- **The grant budget stops new grants.** A client at RFC 14's grant budget is
  offered no new lease or oplock, and keeps those it holds, since SMB has no
  request to return some
  ([RFC 14 §5.5](rfc-14-open-state.md#5.5%20A%20client%27s%20grants%20are%20bounded));
  the profile container a session has used since sign-in keeps its lease.

> decision: closing a moved continuously available share's connections, and
> the ceiling that a persistent handle survives a move only if the repoint
> lands within the client's own retries, are
> [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)'s
> decision; this adapter carries them out. The cost here is that the
> connection's trees on unmoved shares reconnect too. Revisit with RFC 27's
> trigger, the Windows-client row of §18.

> decision: a timed-out break downgrades the grant and keeps the opens, as
> Windows servers do. Closing the holder's opens would end a stalled virtual
> machine's profile disk mid-session — the worst outcome for the reference
> workload — to protect against writes the holder buffered under write
> caching arriving after the waiting client's. Those writes still arrive
> through a valid open, and are ordered with the other client's by arrival,
> like any two uncoordinated writers
> ([RFC 14 §7.1](rfc-14-open-state.md#7.1%20Writers%20that%20do%20not%20coordinate)).
> This is RFC 14 §5.3's own rule for SMB, where the open, not the lease,
> carries the I/O; an NFS delegation's revocation still refuses its later
> writes. Revisit if a workload shows stale buffered writes overwriting another
> client's data after a break timeout.

> ponytail: the waiting node learns a break has ended by re-running the request
> on a backoff (10 ms doubling to 1 s), because the acknowledgement may arrive
> at another node; the cost is up to a second of extra wait. Have the primary
> notify the waiter when those retries show up in `CREATE` latency.

### 6.4 Durable and persistent handles

A durable handle survives a lost connection for a timeout: a user on flaky
Wi-Fi keeps their open documents, and a mounted virtual disk does not vanish
under the desktop. It is held in the primary's memory: a handover of its shard
carries it, a restart or failover of the primary does not. A persistent handle is also stored, so it survives the primary
failing; it is offered only on continuously available shares.

| Request | Granted when | Durability |
| --- | --- | --- |
| durable v1 (`DHnQ`) | a batch oplock or a lease with handle caching is granted | durable, 60 s |
| durable v2 (`DH2Q`), not persistent | as v1 | durable; the client's timeout, 60 s if zero, at most 300 s |
| durable v2 with `PERSISTENT` | the share is continuously available | persistent, same timeout bound |

**Reconnect** (`DHnC`, `DH2C`) is a `CREATE` that **MUST** be routed by the
FileId's persistent half, which names the open and its shard (§6.1), to that
shard's primary, and find the open there. The path in the request is not used
to route: a rename since the disconnect, or a shard move of the path, would
send it to a primary that holds no such open. It succeeds only if every one of
these holds, and is otherwise `STATUS_OBJECT_NAME_NOT_FOUND`:

- the open is durable or persistent and its timeout has not run out;
- the session's principal is the open's;
- the tree's share is the open's ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees));
- the connection's `ClientGuid` is the open's, on every reconnect, v1 or v2;
  every dialect served sends one, since 2.0.2 is refused (§3);
- for v2, the `CreateGuid` is the open's;
- the lease key, if any, is the open's;
- the open is not attached to a live session. One still attached to a session
  on this node was released by `PreviousSessionId`
  ([§4.3](#4.3%20Reconnect%20after%20a%20node%20is%20lost)) before the
  reconnect arrives.

A successful reconnect **MUST** attach the open to the reconnecting session,
in the step that reconnects it
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).
Otherwise the open still names the session that was lost, a second network
blip's disconnect of the new session finds no open of its own, and the open's
timeout never starts again.

**After a primary is lost**, the durable (not persistent) open was held in
memory and is gone, and the reconnect **MUST** be answered
`STATUS_OBJECT_NAME_NOT_FOUND`, as MS-SMB2 answers a reconnect to a server that
restarted. It **MUST NOT** be accepted as a reclaim. A persistent open is a
stored record and reconnects with its lease
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).

**Writes on a persistent open, or on any open on a continuously available
share, are stable.** Each **MUST** be synced to the primary's journal before
it is acknowledged, whatever its write-through flag
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens), §9.4). SMB has no write
verifier: when a primary is lost, a client is never told that unsynced writes
it was answered for went with it, and never resends them. (A loss while the
process keeps running is told, at the open's next `FLUSH`, §9.4.) An open that
survives the primary and reconnects with
its lease after the primary lost unsynced writes would let the client go on as
if those writes were on disk — a silent hole in a virtual disk that shows up as
corruption days later. Making writes on surviving opens stable, and refusing
durable opens that do not survive, removes both halves of that.

![What a reconnect finds after each kind of loss: a volatile open is lost after a network blip, a lost gateway node or a lost primary; a durable handle reconnects after a blip or a lost gateway node, but after the primary is lost it was only in that primary's memory and the reconnect is STATUS_OBJECT_NAME_NOT_FOUND; a persistent handle, offered only on a continuously available share, is stored and reconnects with its lease in all three cases, with every acknowledged write synced](img/rfc22-node-loss.svg)

> decision: a durable v1 or v2 open is not reclaimable across the loss of its
> primary; only a persistent open survives it. Reclaiming a durable open whose
> unstable writes the primary lost would hand the client an open over a gap it
> cannot see. Refusing it makes the client report a failed handle, which an
> application can recover from. The cost: clients on shares that are not
> continuously available lose their open files on a primary failover, as they
> do on a Windows server restart. Revisit only with a per-open record of the
> last synced write, which would let a reclaim be refused only when writes were
> actually lost.

> decision: writes on a continuously available share are stable even through
> an open that is not persistent. The share promises that handles outlive a
> server failure; a client that asked only for a durable handle there is still
> told the share is continuously available, and data acknowledged on such a
> share must outlive what the share says it survives. The cost is one journal
> sync per write on those shares, and for an overwrite of committed bytes a
> share of the next group commit, never a transaction of its own (§9.4).
> Revisit if a measured continuously available workload is limited by sync or
> group-commit latency; the answer then is a faster journal device or a shorter
> group-commit age, not unstable writes.

Example: after a 10 s blip alice-pc reconnects within its 60 s timeout; a
reconnect by bob's session naming alice's FileId is refused, because the
principal differs.

**App instance takeover.** A `CREATE` carrying an app instance id
(`AppInstanceId`) that an open of the same file already holds closes that open
first, unless both carry an app instance version and the new one is not
higher, when the create is refused
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)). The
primary applies it, in the open's own step, only when both hold, from the
`ClientGuid` and the caller's maximal access the adapter passes with the
create:

- the matching open belongs to a different client (its `ClientGuid` differs
  from the connection's); and
- the caller's maximal access to the target includes read
  (`FILE_READ_DATA`).

Otherwise the create **MUST** proceed as if no open matched: it neither closes
the earlier open nor is refused for it, and meets that open's deny mode and
lease like any other create.

> decision: takeover is gated on a different client and on read access, as
> Windows servers gate it. A same-client match is that client's own open,
> which it can close itself; a caller that cannot read the file gains, by
> closing another's open, a denial of service it could not otherwise cause.
> RFC 14 §8.1 holds the same two conditions at the primary; the adapter's part
> is to pass `ClientGuid` and maximal access with every create. Revisit if a
> failover cluster is shown re-attaching from the same `ClientGuid`.

### 6.5 Replay

A client that sent a request and lost the reply re-sends it marked as a replay.
The server must answer it without doing it twice.

- A `CREATE` marked as a replay (`SMB2_FLAGS_REPLAY_OPERATION`) whose durable v2
  request names, by `CreateGuid`, an existing open of the same client returns
  that open (MS-SMB2 3.3.5.9.10), on any node, because the `CreateGuid` is kept
  with the open ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).
- Every other request naming a FileId, on 3.x, is checked at the primary
  against the open's `Channel.Seq` and its two `Channel.Outstanding` counts — requests
  in flight under `Channel.Seq`, and under the sequences before it — fields of
  RFC 14's Open ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), exactly as
  MS-SMB2 3.3.5.2.10 does. "Newer" means a 16-bit difference from
  `Channel.Seq` of at most 0x7FFF, and "older" any other unequal value:
  - **not flagged as a replay**: equal to `Channel.Seq`, it is counted under
    it; newer, the current count is added to the earlier one, the request
    becomes the current one's only request and its sequence becomes
    `Channel.Seq`; older, a `WRITE`, `SET_INFO` or `IOCTL` is refused
    `STATUS_FILE_NOT_AVAILABLE` and any other request runs;
  - **flagged as a replay** (`SMB2_FLAGS_REPLAY_OPERATION`): equal, it runs,
    counted under `Channel.Seq`, only while no request under an earlier
    sequence is still in flight, and otherwise a `WRITE`, `SET_INFO` or
    `IOCTL` is refused `STATUS_FILE_NOT_AVAILABLE`; newer, the current count is
    added to the earlier one and `Channel.Seq` raised as above, and if any
    request under an earlier sequence is still in flight the current count is
    0 and a `WRITE`, `SET_INFO` or `IOCTL` is refused the same way, else the
    request is the current one's only request; older, a
    `WRITE`, `SET_INFO` or `IOCTL` is refused the same way.

  A request leaving the primary lowers the count of the sequence it was
  counted under. So a replay never runs beside the original it repeats: while
  the original is in flight under an earlier sequence, the replay is refused,
  and the client sends it again. `STATUS_FILE_NOT_AVAILABLE` is also the
  status a request held past a freeze's or grace's bound ends with (§16.5).
  `Channel.Seq` is durable with a persistent open, written in the transaction of
  the request that raised it, so a stale replayed `WRITE` is refused after a
  failover too; the counts are never written, and a new primary starts them
  at zero
  ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).
  Any request naming a FileId may raise `Channel.Seq` — a `READ` or
  `QUERY_INFO` as much as a `WRITE` — and most have no transaction of their own
  to carry the raise, so **any** request that raises a persistent open's
  `Channel.Seq` writes the new value in a transaction before it runs. A raise
  happens only when the client moves the open to a new channel after a
  failure, so this costs one transaction per channel move, not per request.
  The gateway node keeps no replay state of its own.
- **No replay is recognised by message ID.** Message IDs are per connection:
  two channels of one session can send two different requests under one
  (`ClientGuid`, session ID, message ID), and a table keyed by that triple would
  answer the second with the first's result, so it would be acknowledged and
  never applied. The request ID a forwarded call carries names no message ID
  (§16.2), and a replay arriving on another channel is matched at the primary only by
  the `CreateGuid`, `LockSeq` and `ChannelSequence` rules above.
- A `LOCK` replay uses the open's lock sequences (`LockSeq`,
  [RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)): a request
  whose index already holds its sequence number succeeds without being applied
  again. It is honoured on 3.x for durable v2 and persistent opens; a
  persistent open's sequences are written with the lock, so the answer
  survives a failover. Resilient handles (`FSCTL_LMR_REQUEST_RESILIENCY`) are
  not offered ([Appendix F](#Appendix%20F%20%E2%80%94%20control%20codes)), so
  2.1 has no lock replay.

### 6.6 Service errors on the wire

Which status each service error is answered with, and whether a request is
held pending first, is §16.5's one table.

### 6.7 Translation to open state

The client, open, lease, lock and watch forms of this chapter map onto
RFC 14's records as §16.4 states, and the identity a session carries as §16.1
states.

## 7. Byte-range locks

A Windows lock covers a byte range of a file and is mandatory: while it is held,
reads and writes of the range by other opens fail, not only other lock requests.
[RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols) enforces
that against every protocol; this section maps the SMB request.

- `LOCK` elements map onto RFC 14 locks under the open: shared or exclusive, a
  64-bit range.
- **A `LOCK` is one service call, however many elements it carries.** Its
  elements are all locks or all unlocks, as the first element's flags say
  (MS-SMB2 3.3.5.14); every element's flags are checked when the arguments are
  decoded, and a request mixing the two, or with a flag combination MS-SMB2
  2.2.26.1 does not list, is `STATUS_INVALID_PARAMETER` before any range is
  touched. The handler then makes one `Lock` or one `Unlock` call with every
  element's range in order
  ([RFC 20 §4.3](rfc-20-adapters.md#4.3%20A%20handler%20makes%20at%20most%20one%20mutating%20call)):
  - a lock list is all or nothing: if any range cannot be granted, the service
    releases the ranges it took for this request before answering, and the
    request is answered as that range's conflict (MS-SMB2 3.3.5.14.2);
  - an unlock list is applied in order and stops at the first range not held,
    answered `STATUS_RANGE_NOT_LOCKED`; the ranges unlocked before it stay
    unlocked (MS-SMB2 3.3.5.14.1);
  - a lock list of more than one element in which any element lacks
    `FAIL_IMMEDIATELY` is `STATUS_INVALID_PARAMETER`, as MS-SMB2 3.3.5.14.2
    recommends, so only a single-element `LOCK` ever waits.

- A conflicting lock asked to fail at once (`FAIL_IMMEDIATELY`) is
  `STATUS_LOCK_NOT_GRANTED`. Without that flag the request goes async, as a
  break does ([§6.3](#6.3%20Breaks)), and its waiter has RFC 14's one
  lifetime ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)): it
  completes on grant, on `CANCEL` (`STATUS_CANCELLED`), or when its open
  closes or its connection ends, and with `STATUS_NETWORK_NAME_DELETED` when
  `Revoked` reports its share removed or moved (§6.3, §16.5).
- **A waiting lock has no deadline.** The adapter **MUST NOT** answer it on a
  timer; it holds no worker and is exempt from the per-call deadline
  ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)).
- **No answer but the grant before `CancelLock`.** Before the adapter answers a
  waiting lock with anything but the grant `LockWaitOver` reports — on
  `CANCEL`, on close, when the connection ends — it **MUST** call `CancelLock`
  and answer only once that returns. If `CancelLock` reports the lock granted
  first, the request is answered success, even on `CANCEL`. Without the order,
  a lock is granted after its request was answered failed: an orphan lock no
  client knows it holds, refusing every other client of the range until the
  open closes.
- A read or write across a range another open has locked is
  `STATUS_FILE_LOCK_CONFLICT`.
- A lock request from another client on a file under a grant with read or
  write caching — an SMB lease or oplock, or an NFS delegation — recalls the
  grant to none first
  (MS-FSA 2.1.5.7; [§6.3](#6.3%20Breaks)), and waits pending while it does.
- Unlocking a range not held is `STATUS_RANGE_NOT_LOCKED`.
- A zero-length lock is held. Its last byte is offset − 1 (MS-FSA 2.1.4.10), so
  one at X conflicts only with a lock or I/O range holding both X − 1 and X,
  and one at 0 conflicts with nothing
  ([RFC 14 §2.3](rfc-14-open-state.md#2.3%20Lock)).

> decision: a mixed lock-and-unlock array is refused before any range is taken,
> where MS-SMB2 lets a lock array take its earlier ranges and keep them when an
> unlock element fails it. Checking every element at decode keeps the call all
> or nothing, which the one-call rule needs; the client sees the same
> `STATUS_INVALID_PARAMETER` either way and only the earlier ranges' state
> differs. Revisit if a client is shown to send such arrays and depend on the
> ranges taken before the bad element.

## 8. Change notification

Explorer asks to be told when a directory changes (`CHANGE_NOTIFY`) so its
window refreshes by itself. The request waits until something changes. Each
request is backed by an RFC 14 watch, fed by the service's `Notify` callback.

- The first request on an open registers an RFC 14 watch; later requests reuse
  it. The request's completion filter is the watch's mask, and `WATCH_TREE` its
  recursion.
- A request goes async and completes with the changes `Notify` delivered
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)).
- Changes arriving with no request outstanding are buffered per open up to the
  request's `OutputBufferLength`; past it, the next request completes
  `STATUS_NOTIFY_ENUM_DIR`, and the client re-lists the directory.
- Closing the open, or the share's removal or move reported through `Revoked`
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)), ends the watch; a request
  outstanding then completes
  `STATUS_NOTIFY_CLEANUP`.
- **A name in a notification is spelled as a listing spells it.** Every name
  in a `FILE_NOTIFY_INFORMATION` entry **MUST** pass through §12.3: a name with
  a character Windows reserves is reported in its private-use spelling, and a
  name §12.3 hides is not reported at all. A rename between a hidden and a
  shown name is reported as the shown name's removal or addition only.
  Otherwise a notification would name a file the listing does not, or show
  bytes the client cannot open.
- **A recursive watch spans shards** (cluster). A watch lives at its
  directory's primary, and a subtree below the directory may be its own shard
  with another primary. The adapter registers one watch; RFC 14 leaves a marker
  at the primary of each shard below it, including one created or moved there
  later, and a change there that the filter matches completes the request
  `STATUS_NOTIFY_ENUM_DIR` rather than with the change, whose names are another
  primary's
  ([RFC 14 §2.5](rfc-14-open-state.md#2.5%20Watch)). The client re-lists, as
  it does after an overflow. A change is never silently missed for being in
  another shard; on one node every change arrives with its detail.

## 9. File information

Windows reads and sets file and volume metadata — times, sizes, attributes,
streams, free space — through numbered information classes, each a fixed
layout. The supported ones are listed in
[Appendix E](#Appendix%20E%20%E2%80%94%20information%20classes); this section
holds the rules a table cannot.

### 9.1 What is reported

- File, directory, volume, security and quota classes map onto RFC 7's records
  as [Appendix E.1](#E.1%20Supported) lists.
- The volume reports itself as NTFS, with flags that say what is supported
  ([Appendix E.2](#E.2%20Volume%20attributes)).
- **The `FileID` never reaches the wire.** RFC 7's `FileID` is what a file
  handle carries, and for NFSv3 a handle is a bearer token, so knowing a
  file's `FileID` is most of the way to reaching it without a lookup
  ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)).
  Every SMB field that reports a file id — `FileInternalInformation`,
  `FileIdInformation`, the directory classes' ids, the `QFid` create context
  and the object-id control codes — **MUST** carry RFC 7's ids
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)): the 64-bit
  `Number`, or the 128-bit id formed from a 64-bit keyed digest of the
  `ShareID` under an installation secret followed by `Number`. Neither contains
  a bit of the `FileID` or can be turned back into one. A file seen through a
  snapshot reports the snapshot's ids instead (§15).

> decision: the file system name is `NTFS`, because applications and installers
> refuse other names; the flags say what is supported. The risk is a client
> inferring from the name a feature this server lacks. Report another name only
> if a client is shown to do that.

### 9.2 Not supported

Short 8.3 names, valid-data length, setting the volume label, setting object
ids and mandatory labels are refused or answered empty;
[Appendix E.3](#E.3%20Not%20supported) gives each answer and why.

### 9.3 Timestamps, allocation and sparse files

**Timestamps.** A client may freeze one of a file's times for as long as it
holds an open, so that its own writes do not move it.

- In `FileBasicInformation`, −1 in a time suspends that time's automatic update
  for the rest of the open's life; −2 resumes it (MS-FSA 2.1.5.14.2); an
  explicit time suspends it as −1 does. Zero means "leave unchanged".
- The suspension **MUST** be held with the open at the file's primary
  (`Open.Suspended`, set through the service's `SetAttr`, which runs RFC 14's `Opens.Update`,
  [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), not in the adapter, because
  the primary decides each write's times when it admits the write: the journal
  record carries the modification time the write sets, or none when its open
  suspended it, and the existence commit and a replay take `mtime` from the
  records ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)).
- A write through a suspending open **MUST NOT** move `LastWriteTime` or
  `LastAccessTime` while a write through another open of the same file still
  does.
- **The change time is never suspended.** A −1 or −2 in `ChangeTime` is
  accepted and changes nothing: the stored change time, which NFS reports as
  ctime, **MUST** move on every write whatever any SMB open suspended
  ([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)). NFSv3
  clients revalidate their caches by ctime, so a frozen ctime would leave every
  NFS reader of a profile disk serving stale data after SMB writes.
- The suspension lasts as long as the open's record at the primary
  ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)): it survives a reconnect
  after a network drop, and a persistent open's survives the primary.

> decision: suspension is open state, not adapter state. In the adapter it
> could only rewrite times after the commit set them, and a client reading
> through another node would see them change twice. The cost is one field per
> open at the primary, stored only with a persistent open.

> decision: `ChangeTime` suspension is accepted and ignored. MS-FSA lets a
> client freeze it, but the same field is the ctime NFS clients use to detect
> change, and one stored time cannot be both frozen for one protocol and moving
> for the other. A Windows client that reads `ChangeTime` back after
> suspending it sees it move; no supported application is known to depend on
> that. Revisit, with a per-protocol change time, if one is shown to.

**Allocation size.**

- The allocation size reported is `Charged`
  ([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem))
  rounded up to 4096.
- Setting it below the end of file truncates (MS-FSA). Setting it above the
  end of file, by `FileAllocationInformation` or the `AlSi` create context, is
  accepted as a hint: it succeeds, reserves nothing, and leaves the end of file
  and the reported allocation where they were.

> decision: allocation size above the end of file is a hint, not a
> reservation, because nothing below the adapter reserves space: the service
> offers no `Allocate` ([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), and
> [RFC 21 §9.3](rfc-21-nfs.md#9.3%20NFSv4.2%20operations) refuses NFS's
> `ALLOCATE` for the same reason. Refusing it would fail every Windows copy,
> which sets the allocation size before writing; accepting it costs only the
> promise that the later writes cannot fail for space, which an application
> meets as `STATUS_DISK_FULL` on the write instead. Make it a reservation when
> a lower RFC defines one charged against the share's limit.

**Sparse files.** `FSCTL_SET_SPARSE` sets the sparse DOS attribute in `Flags`
and nothing else: every file may have holes, sparse or not, and holes are
[RFC 6](rfc-6-block-metadata.md)'s.

### 9.4 Writes, flushes and stability

- `WRITE` is an unstable `Write`, except as below.
- It is a stable `Write` with `SMB2_WRITEFLAG_WRITE_THROUGH`, on an open made
  with `FILE_WRITE_THROUGH`, on a persistent open, and on any open on a
  continuously available share (§6.4).
- `FLUSH` is `Commit`.
- **A stability reply waits for overwrites' existence.** A stable write, and a
  `FLUSH`, **MUST** be answered once the journal has synced every record in its
  range and once the existence commit of every pending write in its range that
  overwrites committed content — the writes that need an overwrite record — has
  committed; an append or hole fill does not wait for its existence commit,
  and a waiting reply joins the next group commit rather than issuing one of
  its own
  ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)).
  The journal record carries the data and the modification time the write sets
  ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)). A profile disk on a
  continuously available share, whose steady state is overwrites of committed
  64 KiB pieces, therefore pays one journal sync per write and a share of one
  group commit, which batches across files, never a transaction of its own.
  While the metadata store does not commit
  ([RFC 8 §11.2](rfc-8-engine.md#11.2%20Every%20condition%20in%20RFC%200%20%C2%A710%20has%20its%20engine%20behaviour%20here)),
  such a reply is held pending and re-driven up to `smb_pending_cap`, and only
  there answered `STATUS_UNEXPECTED_IO_ERROR`
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks), §16.5).
- **A loss without a restart fails the next `FLUSH` of every open across it.**
  The journal can lose acknowledged, unoffloaded writes while the process runs —
  a record dropped as corrupt or a failed sync window, the loss events
  [RFC 1 §3.8](rfc-1-journal.md#3.8%20Loss%20events) names — and SMB has no write
  verifier to tell the client. A `FLUSH` through an open whose `LossSeen` is
  below the file's loss sequence is `ErrLost`, answered
  `STATUS_UNEXPECTED_IO_ERROR`, and brings `LossSeen` up to date, so each open
  that was open across the loss learns of it once, at its next flush, and later
  flushes succeed
  ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).
  A durable open that reconnects after such a loss carries the same mark, so
  its next flush fails too. The loss sequence lives for one process, so a
  persistent open reinstated after its primary's restart or failover, or
  imported at another installation by a namespace move, has `LossSeen` reset
  to 0
  ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)):
  a value carried over would sit above the new process's sequence and let a
  flush succeed over its loss.
- A write refused for capacity is held pending up to `smb_pending_cap` while
  the cause is transient, and answered as §16.5 says.
- A short read follows [RFC 17 §5.2](rfc-17-vfs.md#5.2%20Read), except that
  a read meeting a remote outage below its `MinimumCount` is held pending, not
  answered short (§16.5).

### 9.5 Extended attributes

SMB extended attributes (EAs) are RFC 7 xattrs, the same records NFS's xattr
operations reach ([RFC 21 §9.3](rfc-21-nfs.md#9.3%20NFSv4.2%20operations)). SMB
compares EA names without case: a set whose name differs only in case from a
stored name replaces that one. An EA set or removal through a FileId passes the
open to the service as its `Target`'s `Open`
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), as a rename does (§11), so it
is decided by the open's grant, an intent handle's privilege included.

## 10. Control codes

A file-system control code (FSCTL) is SMB's catch-all for operations beyond
read and write: server-side copy, zeroing a range, symlinks, snapshot lists.

- Server-side copy (`FSCTL_SRV_COPYCHUNK`, and cloning by
  `FSCTL_DUPLICATE_EXTENTS_TO_FILE`) is the service's `Copy` and `Clone`, the
  engine's clone ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)), so a copy made
  in Explorer between files of one namespace never crosses the network:
  - **copychunk is not atomic.** Each request runs as bounded chunks, each one
    small atomic clone, so only the current chunk's source range is frozen;
    the response reports the chunks and bytes written, which may be short of
    the request, and the client continues from there;
  - **duplicate-extents is atomic.** The whole range is one clone, answered
    only when done; one longer than `clone_max_len`
    ([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings),
    1 GiB by default) is `STATUS_INVALID_PARAMETER` (`ErrInvalid`), and the
    client copies the range itself;
  - a copy or clone whose source and destination are one file and whose
    ranges overlap **MUST** be `STATUS_INVALID_PARAMETER` (`ErrInvalid`) and
    change nothing;
  - a copy or clone between shares in different namespaces **MUST** be
    `STATUS_NOT_SUPPORTED` (`ErrCross` (`CrossNamespace`)) and change nothing, so the
    client falls back to reading and writing: no chunk can be adopted across
    namespaces, and shares get their own namespace by default, so an Explorer
    copy between two such shares goes through the client;
  - a write into the source or destination range of a running clone or chunk
    is held at the service until that clone or chunk ends and then runs
    ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)); the adapter sends
    an interim `STATUS_PENDING` from its own timer while the call waits
    ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)), and only if the write's own deadline passes
    first is it answered from `ErrDelay` as §16.5's clone row says. The same
    holds for a `SET_INFO` of the end of file, `FSCTL_SET_ZERO_DATA`, a copy
    or clone whose destination is such a range, and a delete or last `CLOSE`
    that would release a running clone's destination. The adapter counts each
    such wait against its connection's floors and the node's hold pool (§3.3),
    so the connection holds at most those floors plus what the pool lends it,
    whether the service or the adapter is holding each request; a Lost
    source run reads Lost at the
    destination, never zeros; a clone refused at admission leaves the
    destination's prior content, and one begun is resumed after a crash,
    never undone.
- Zeroing a range is `Deallocate`; asking which ranges hold data is `Seek`,
  answered from the engine's allocation, where a zero ref under a newer
  overwrite record counts as data
  ([RFC 17 §5.2](rfc-17-vfs.md#5.2%20Read)).
- Reparse-point codes are symlinks and junctions
  ([§12.4](#12.4%20Reparse%20points)); object-id codes return the derived
  128-bit file id of §9.1, never the `FileID`; the snapshot listing is
  [§15](#15.%20Previous%20Versions).
- Offloaded data transfer (ODX), resilient handles, compression, trim, integrity
  streams and shared virtual disks are refused `STATUS_NOT_SUPPORTED`.

[Appendix F](#Appendix%20F%20%E2%80%94%20control%20codes) lists every code
with its mapping and limits.

> decision: no ODX. An ODX copy is done with a token that stands for a range of
> a file: whoever holds it can read the range, and it outlives the open that
> made it, so it would need its own record, expiry and revocation at the
> primary. Copychunk and duplicate-extents give the same server-side copy
> within a namespace without one. Offer ODX when a workload shows copychunk limiting a copy that
> ODX would not.

## 11. Security descriptors

A Windows security descriptor holds a file's owner and group, its access
control list (DACL, the discretionary ACL: who may do what), and its system ACL
(SACL: which accesses are audited). Here it is RFC 7's one ACL, whose model and
evaluation are [RFC 19](rfc-19-authorization.md#2.%20The%20model)'s, spelled
through the one translation both adapters use
([RFC 19 §5.1](rfc-19-authorization.md#5.1%20One%20translation%2C%20two%20spellings)).
This section is that spelling.

**The parts.**

| Part | Model | To read | To write |
| --- | --- | --- | --- |
| owner SID | the File's `Owner` | read the ACL | change the owner, under [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)'s rules for the value |
| group SID | the File's `Group` | read the ACL | change the owner, under RFC 7 §7.4 |
| DACL | the access list and its flags | read the ACL | write the ACL |
| SACL | the audit list and its flags | `ACCESS_SYSTEM_SECURITY` | `ACCESS_SYSTEM_SECURITY`, and write the ACL in the call's ceiling |

- The rights are asked of the chokepoint through the open the request came on
  ([RFC 17 §4.6](rfc-17-vfs.md#4.6%20One%20chokepoint)), so the open's grant
  decides, as Windows' set-security does. A query returns only the parts its
  security-information bits ask for, and never a label, attribute or scope
  part. A set changes only the parts its security-information bits name,
  whatever else the descriptor holds, and the parts it names change together,
  in one transaction, with `Mode`
  ([RFC 19 §4.3](rfc-19-authorization.md#4.3%20Setting%20mode%20and%20ACL)).
- **Owner and group SIDs** are RFC 18's principals
  ([RFC 18 §4.4](rfc-18-identity.md#4.4%20SIDs)), spelled as §16.3 says; a
  deleted one is shown as the null SID `S-1-0-0`. On a set, one no principal
  holds is `STATUS_INVALID_OWNER` or `STATUS_INVALID_PRIMARY_GROUP`; it is never
  ignored with the rest of the set applied.
- **The SACL is the ACL's audit list.** Reading or writing it needs
  `ACCESS_SYSTEM_SECURITY` on the open, which is granted only to a caller whose
  identity has `admin` set, never by the file's ACL, by ownership or by
  privilege alone
  ([RFC 19 §3.5](rfc-19-authorization.md#3.5%20Privilege%20and%20the%20system-security%20right)).
  A request for it by anyone else is `STATUS_PRIVILEGE_NOT_HELD`. NFS gates the
  same entries the same way ([RFC 21 §11.3](rfc-21-nfs.md#11.3%20Namespace)), so
  neither protocol is a way round the other. Audit entries never grant or deny;
  they select the access events the service emits
  ([RFC 19 §3.6](rfc-19-authorization.md#3.6%20Audit%20entries%20select%20events)).
- **A descriptor sent with a `CREATE`** (the `SecD` context) is the new file's
  ACL, owner and group, set under the same rules in the create's transaction,
  in place of the inherited ACL. It carries no security-information bits, so
  its parts are those it holds; a part it does not hold is the one the create
  would give, so a `SecD` without a SACL keeps the inherited audit list
  ([RFC 19 §4.4](rfc-19-authorization.md#4.4%20Inheritance)).

**Control bits.** The descriptor's control bits map to RFC 19's list flags
([§2.6](rfc-19-authorization.md#2.6%20ACL%20flags%2C%20null%20and%20empty)):

| List flag | DACL | SACL |
| --- | --- | --- |
| protected | `SE_DACL_PROTECTED` | `SE_SACL_PROTECTED` |
| auto-inherited | `SE_DACL_AUTO_INHERITED` | `SE_SACL_AUTO_INHERITED` |
| defaulted | `SE_DACL_DEFAULTED` | `SE_SACL_DEFAULTED` |

`SE_DACL_PRESENT` with no DACL is the null list, stored as allow `EVERYONE@`
every right and read back so. The protected flag of a list is its descriptor's
control bit, set or clear, for a list the set names. The protected and
unprotected security-information bits (`PROTECTED_DACL_SECURITY_INFORMATION`
and its siblings) are an API's, which the client turns into that control bit;
they are not among the bits a set request carries (MS-SMB2 2.2.39).

**Inheritance computed at set.** A set may carry `SE_DACL_AUTO_INHERIT_REQ` or
`SE_SACL_AUTO_INHERIT_REQ`, asking for inheritance to be computed. Such a set
recomputes that file's inherited entries from the current lists of the directory
the open was resolved through (its `via`,
[RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)),
by RFC 19 §4.4's rules, read in the set's transaction. It keeps the explicit
entries the client sent, and stores explicit before inherited. A set with no
`via` — on a share's root, or through an open by file ID — stores the entries as
sent. A set without that request stores the entries as sent, inherited flags
included.

**Entries.**

- Allow, deny, audit and alarm entries of the basic forms are RFC 19's kinds
  ([§2.2](rfc-19-authorization.md#2.2%20Entry%20kinds)). The DACL holds allow
  and deny; the SACL holds audit and alarm.
- Masks are entry rights after the generic mapping (§6.1). Each right is the SMB
  right of the same bit
  ([RFC 19 §2.4](rfc-19-authorization.md#2.4%20Rights)):

  | Right | SMB name (on a directory) |
  | --- | --- |
  | read data | `FILE_READ_DATA` (`FILE_LIST_DIRECTORY`) |
  | write data | `FILE_WRITE_DATA` (`FILE_ADD_FILE`) |
  | append | `FILE_APPEND_DATA` (`FILE_ADD_SUBDIRECTORY`) |
  | read / write extended attributes | `FILE_READ_EA`, `FILE_WRITE_EA` |
  | execute | `FILE_EXECUTE` (`FILE_TRAVERSE`) |
  | delete a child | `FILE_DELETE_CHILD` |
  | read / write attributes | `FILE_READ_ATTRIBUTES`, `FILE_WRITE_ATTRIBUTES` |
  | delete, read the ACL, write the ACL, change the owner, synchronize | `DELETE`, `READ_CONTROL`, `WRITE_DAC`, `WRITE_OWNER`, `SYNCHRONIZE` |
  | system security; the maximal flag | `ACCESS_SYSTEM_SECURITY`; `MAXIMUM_ALLOWED` |

- Flags map by name
  ([RFC 19 §2.5](rfc-19-authorization.md#2.5%20Entry%20flags)), never by bit:

  | Entry flag | SMB bit |
  | --- | --- |
  | file inherit | `0x01` (`OI`) |
  | directory inherit | `0x02` (`CI`) |
  | no propagate | `0x04` (`NP`) |
  | inherit only | `0x08` (`IO`) |
  | inherited | `0x10` |
  | successful access | `0x40` |
  | failed access | `0x80` |

- Order is kept ([RFC 19 §2.7](rfc-19-authorization.md#2.7%20Order)). An
  explicit entry after an inherited one is stored where it was sent.

**Owner and group entries.** Windows has no word for "whoever owns this file".
It names the current owner's account directly, and uses CREATOR OWNER,
inherit-only, for future children. The translation keeps `OWNER@` and a named
owner entry apart this way:

- **Read.** An effective `OWNER@` entry with no inherit flags is spelled with the
  owner's SID. An inherit-only `OWNER@` entry is spelled CREATOR OWNER. An
  `OWNER@` entry both effective and inheritable is spelled as two: the owner's
  SID with no inherit flags, then CREATOR OWNER inherit-only with the inherit
  flags. `GROUP@` likewise, with the group's SID and CREATOR GROUP.
- **Write.** First, a **pair** is folded into one entry: two adjacent entries of
  the same kind and mask, and the same flags but the inherit flags, the first
  naming the SID that will be the file's owner after the set with no inherit
  flags, the second naming CREATOR OWNER, inherit-only, with at least one
  inherit flag, are stored as one `OWNER@` entry carrying the second's inherit
  flags without inherit-only. The group's SID followed by CREATOR GROUP folds to
  `GROUP@` alike. Then an **allow** with no inherit flags naming the owner's SID
  is stored as `OWNER@`, and one naming the group's SID as `GROUP@`. A deny
  naming the owner's or group's SID stays a named deny. Every other entry is
  stored with the principal it names.

The fold makes a write the inverse of a read for an inheritable `OWNER@`, and
changes no decision: on the file both forms grant the owner the same rights, and
below it CREATOR OWNER's copy names `OWNER@`
([RFC 19 §4.4](rfc-19-authorization.md#4.4%20Inheritance)), as the folded
entry's does. The NFS spelling folds the same pair
([RFC 21 §11.3](rfc-21-nfs.md#11.3%20Namespace)), so no stored ACL holds an
unfolded pair. A write is not a read's inverse in one case: a stored named allow
with no inherit flags for the principal that now owns the file is shown as the
owner's SID and written back as `OWNER@`, and it grants the same until the next
`chown`.

> decision: an effective allow naming the owner's SID is stored as `OWNER@`.
> Without it, every Explorer edit of a file with a mode-derived ACL turns the
> owner's entry into a named entry for alice, which a later `chmod 750` limits to
> the group bits and a `chown` leaves granting alice. The cost: an allow a
> Windows user meant for alice personally follows the file to its next owner.
> Denies are left named, so a deny written for alice keeps denying alice after a
> `chown`; the cost there is that the mode-implied `deny OWNER@` of a rare mode
> such as 0044 comes back as a named deny, which fails closed for a new owner.
> Overturned if a workload is shown to `chown` files whose Windows ACLs name the
> old owner and expect that owner to keep access.

**What a set may carry.** A set the model cannot hold is refused whole and
nothing is stored ([RFC 19 §2.8](rfc-19-authorization.md#2.8%20What%20a%20stored%20ACL%20may%20hold)):

| Descriptor content | Answer |
| --- | --- |
| object entries (`ACCESS_ALLOWED_OBJECT_ACE` and its siblings) | `STATUS_INVALID_SECURITY_DESCR` |
| callback (conditional) entries, allow, deny or audit | `STATUS_INVALID_SECURITY_DESCR` |
| mandatory-label, resource-attribute and scoped-policy entries, and the label, attribute and scope security-information parts | a query returns none; a set carrying any is `STATUS_INVALID_SECURITY_DESCR` |
| an entry kind or revision the model does not hold; an audit entry in the DACL or an allow in the SACL | `STATUS_INVALID_SECURITY_DESCR` |
| everything else RFC 19 §2.8 refuses — a list over its limit, a generic bit left in a mask, an entry naming a SID no principal holds, a deny naming a well-known principal that matches no caller | `STATUS_INVALID_SECURITY_DESCR` |
| an owner or group SID no principal holds | `STATUS_INVALID_OWNER`, `STATUS_INVALID_PRIMARY_GROUP` |

> decision: an unsupported entry fails the whole set rather than being skipped.
> Skipping a callback deny grants what it denied; skipping an allow quietly
> withdraws a grant. Both look like success to the client. Overturned only by
> supporting the kind; a tool that sets one is told so, and can fall back.

**Who may read and change a descriptor** is the chokepoint's decision, not the
adapter's: the file's owner holds `READ_CONTROL` and `WRITE_DAC` whatever the
DACL denies, unless an entry for owner rights applies, so an owner can always
repair a DACL that locks it out
([RFC 19 §3.1](rfc-19-authorization.md#3.1%20One%20request%2C%20in%20order),
step 5). An open asking `MAXIMUM_ALLOWED`
([RFC 19 §3.4](rfc-19-authorization.md#3.4%20Maximal%20access%20and%20an%20open%27s%20grant)),
the `MxAc` create context and `MaximalAccess` are answered from
`Files.Granted`, through the service's `Access`
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), to which the adapter passes
the directory the open resolved through, as for `DELETE` at open (§6.1), so the
`DELETE` granted through the parent is in the answer.

**Setting the owner or group** follows
[RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved):
only a privileged caller may set the owner to another principal, and over SMB
only through a handle opened with backup or restore intent; an unprivileged
caller may set it only to itself, and only with `WRITE_OWNER`, which an ACL
grants and which is not a privilege; the owner, or a caller with that right,
may set the group only to one it is a member of; anything else is `ErrPerm`,
`STATUS_ACCESS_DENIED`.

**The backup and restore privileges are the one privileged flag.** Which
callers hold it is
[RFC 18 §6.5](rfc-18-identity.md#6.5%20What%20the%20proof%20proved%3A%20authenticated%2C%20admin%2C%20privileged)'s,
and what it lets a decision grant is
[RFC 19 §3.5](rfc-19-authorization.md#3.5%20Privilege%20and%20the%20system-security%20right)'s.
On the wire: take-ownership is not among them — it is the `WRITE_OWNER` right,
decided by the DACL; a `CREATE` with `FILE_OPEN_FOR_BACKUP_INTENT` sets
`OpenRequest.Backup` (§16.4), and the service marks the open privileged when its
caller is, so every later request through the handle that `CREATE` made carries
that privilege; every request of the session through any other handle is
decided as anyone's is; and `MaximalAccess` and the `MxAc` context are computed
without the flag, even on an intent handle. So a backup tool in backup mode
reads and restores files their DACLs would refuse it, and `robocopy /B /SECFIX`
restores owner, group and DACL alike.

**A request through a handle uses the handle's grant.** A security descriptor
set, a rename, an EA set or removal (§9.5) and a delete, a delete on close's
included (§13), through a FileId pass the open to the service as the `Target`'s
`Open` (an `OpenRef`, [RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), so the
access the open was granted — the privileged flag of an intent handle, and the
`DELETE` granted at open through the parent (§6.1) — applies, and is not
re-checked against the file. A rename still checks, at the rename, that the
caller may add an entry to the target directory (MS-FSA 2.1.5.14.11).

> decision: privilege needs backup or restore intent over SMB, as Windows
> servers apply their backup and restore privileges, and then covers every
> request through the intent handle. Granting it to every open of an `admin`
> session would let a helpdesk group made `admin` for a backup tool's backup
> mode bypass every DACL in Explorer, and a deny-write protecting a template
> would stop no overwrite. The cost: an `admin` who wants the bypass must ask
> for it, as on Windows. Revisit only if a supported backup tool is shown to
> omit the intent flag.

> decision: setting the owner to another principal is narrower over SMB than
> on Windows, and fails closed. Windows reads `SeRestorePrivilege` from the
> caller's token when the owner is set, whatever the open's intent, and its
> take-ownership privilege grants `WRITE_OWNER` in the access check; here an
> `admin` sets another owner only through an intent handle, and the
> take-ownership privilege does not exist. So an administrator's
> `icacls /setowner` without backup intent, or taking ownership of a file
> whose DACL excludes the administrator, is `STATUS_ACCESS_DENIED`. The
> reasoning above is about bypassing the DACL at open; for the owner it is
> only that one rule is simpler to audit than two. Overturn when an
> administrator's `icacls /setowner` or take-ownership case is reported
> failing.

## 12. Names

A stored name is bytes, shared by NFS and SMB; SMB carries UTF-16 and reserves
some characters. This section says how a stored name looks on Windows.

### 12.1 Streams

A Windows file can carry named alternate data streams beside its main content.

- A path component `name:stream` or `name:stream:$DATA` opens the named stream
  (RFC 7's `Stream` file); `name::$DATA` and `name` open the file.
- Any other type after the second colon is `STATUS_OBJECT_NAME_INVALID`.
- Stream names compare without case on every share, folded unit for unit by
  the share's upcase table, or on a case-sensitive share by the first table
  [RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case) names.
- `FileStreamInformation` lists `::$DATA` (files only) and `:name:$DATA` for
  each stream.

### 12.2 Case

Case rules are the share's ([RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case)).
SMB adds no folding of its own. The SMB 3.1.1 POSIX extensions are not offered,
so an SMB client cannot ask for case-sensitive lookup on a case-insensitive
share.

### 12.3 Names SMB cannot carry

A stored name is bytes ([RFC 7 §3.2](rfc-7-namespace-metadata.md#3.2%20A%20name%20is%20bytes%2C%20and%20it%20is%20validated%20at%20the%20boundary));
SMB carries UTF-16. Three cases:

1. **Valid UTF-8 with no character Windows reserves**: converted both ways.
2. **Valid UTF-8 containing a reserved character** — a control character, one of
   `" * : < > ? \ |`, or a trailing space or period — is shown with each such
   character replaced by its private-use code point
   ([Appendix B](#Appendix%20B%20%E2%80%94%20private-use%20mapping)); an SMB
   name containing those code points is converted back. A stored name that
   already contains one of those code points is treated as case 3.
3. **Not valid UTF-8**, or a name case 2 cannot round-trip: **hidden** from SMB.
   It is not listed, cannot be opened by name, and a directory holding only such
   names is not empty to `Unlink` (`STATUS_DIRECTORY_NOT_EMPTY`).

A UTF-16 name with an unpaired surrogate is `STATUS_OBJECT_NAME_INVALID`.

**Names SMB refuses to create.** A name an SMB client sends to create or rename
to **MUST** be refused with `STATUS_OBJECT_NAME_INVALID` when it contains a
character MS-FSCC 2.1.5.2 forbids in a component — a control character or one
of `" * / : < > ? \ |` — or when it is a reserved device name, from the one
list [RFC 7 §3.8](rfc-7-namespace-metadata.md#3.8%20Names%20every%20protocol%20of%20a%20share%20can%20use)
keeps: `CON`, `PRN`, `AUX`, `NUL`, `COM0`–`COM9` or `LPT0`–`LPT9`, compared
without case, alone or followed by a period and anything (`con.txt`). Windows clients cannot
open such a name through their own path parsing, so one made over SMB would
be a file its own creator cannot reach. A name holding the private-use code
points of Appendix B is not refused. **On a `posix` share** it is case 2's
spelling of a reserved character, and is stored with that character. **On a
`windows` share**, whose rule refuses the reserved characters for every
protocol ([RFC 7 §3.8](rfc-7-namespace-metadata.md#3.8%20Names%20every%20protocol%20of%20a%20share%20can%20use)),
cases 2 and 3's mapping is not applied: such a name, like any holding the
private-use code points U+F001–U+F07F, is stored and shown with the code points
themselves, as a Windows server stores them, so a Mac saving
`Q3: draft.docx` creates `Q3` U+F022 ` draft.docx` and sees it again; no stored
name there can hold a reserved character to map. A device name made over NFS is
listed and openable over SMB, since refusing it there would hide a file
another protocol made under a valid name.

Example, on a `posix` share: build01 creates `a:b` over NFS; alice-pc sees it with the colon shown
as U+F022 and can open it. A name in a legacy 8-bit encoding is not shown on
alice-pc at all.

> decision: hide rather than escape. Any escape of arbitrary bytes into UTF-16
> loses bytes or collides with a name someone could type, and RFC 7 forbids
> rewriting a name; a hidden name aliases nothing. The cost is files a Windows
> user cannot see. The private-use mapping is the exception: it is reversible,
> and SMB clients on other platforms already use it. Revisit when a
> mixed-protocol share is shown to hold names in a legacy encoding; the answer
> then is a per-share character set, not an escape.

### 12.4 Reparse points

A reparse point is how Windows marks a symlink or a junction (a directory
link).

- Symlinks are shown as `IO_REPARSE_TAG_SYMLINK` and junctions as
  `IO_REPARSE_TAG_MOUNT_POINT`; both are RFC 7 `Symlink`s.
- **A target is translated, both ways.** A stored target is a POSIX path. In a
  symlink reparse buffer and in the symlink error response, every `/` **MUST**
  be shown as `\`, each component spelled as §12.3 spells names, and
  `SYMLINK_FLAG_RELATIVE` **MUST** be set exactly when the stored target does
  not begin with `/`. A target set over SMB is stored with `\` turned to `/`;
  one that is absolute in Windows terms (a drive letter, or `\??\`) is
  stored as given and resolves nowhere over NFS. A target with a component
  case 3 would hide is refused on query with
  `STATUS_IO_REPARSE_DATA_INVALID`, never shown altered.
- The server does not follow symlinks for the client: a link is always
  returned to the client to resolve.

> decision: symlinks are translated, not followed server-side. A Windows
> client resolves a symlink found on a share only if its local policy allows
> remote-to-remote links, which is off by default, so a mixed-protocol share's
> NFS-made links do not resolve for Windows clients left at their defaults.
> Following links on the server instead would let a link point outside the
> share, or across shares, past the share's admission, and would show Windows
> a file where NFS sees a link. The ceiling is Windows clients' own setting.
> Revisit, with resolution confined to the share, when a mixed-protocol
> workload needs NFS-made links followed by default Windows clients.
- Opening one without `FILE_OPEN_REPARSE_POINT` returns
  `STATUS_STOPPED_ON_SYMLINK` with the symlink error response (MS-SMB2
  2.2.2.2.1), and the client resolves it.
- Other tags are refused ([RFC 7 §2.1](rfc-7-namespace-metadata.md#2.1%20File)).

## 13. Rename and delete with open handles

Windows refuses to rename or delete a file in use in ways NFS does not. RFC 14
checks MS-FSA's rules at the primary, so they hold whichever protocol has the
file open.

- `FileRenameInformation` carries a path from the share root, resolved like a
  `CREATE`'s.
- The renaming open needs `DELETE` access, which every other open of the file
  must have shared.
- With `ReplaceIfExists`, a target that is open by anyone is
  `STATUS_ACCESS_DENIED`; without it, an existing target is
  `STATUS_OBJECT_NAME_COLLISION`.
- A directory with an open file or directory directly inside it cannot be
  renamed or deleted. RFC 14 refuses it with `ErrShareViolation`
  ([RFC 14 §9.5](rfc-14-open-state.md#9.5%20Open%20children%20refuse%20an%20SMB%20rename%20or%20delete%20of%20their%20directory)),
  which the adapter answers on this request as Windows does,
  `STATUS_ACCESS_DENIED`, not `STATUS_SHARING_VIOLATION`.
  Opens deeper in the tree are not checked; RFC 14 names that ceiling and its
  upgrade.
- A delete is a pending delete set by disposition or `FILE_DELETE_ON_CLOSE`,
  and is RFC 14's ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close));
  a later `CREATE` of the name is `STATUS_DELETE_PENDING`. The unlink it runs
  at the last close, run by the service's `Close`, carries the open that set
  it (§11), so it is
  authorised by that open's grant — the `DELETE` granted through the parent,
  or an intent handle's privilege — never re-checked without them. A deferred
  unlink still refused at the close is logged and counted
  ([RFC 17 §7](rfc-17-vfs.md#7.%20Observability)'s
  `vfs_deferred_unlink_refusals_total`) and the file stays: the client
  has already been answered for its `CLOSE`, so nothing reaches it
  ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)).

Example: while alice-pc holds `profiles/alice/ODFC_alice.vhdx` open, renaming
`profiles/alice/` over SMB is refused; an NFS `RENAME` of it succeeds
([RFC 21 §7.3](rfc-21-nfs.md#7.3%20Names)).

## 14. Witness

Witness (MS-SWN) tells a client of a continuously available share which server
addresses are up, and to move before a node is drained, so it reconnects at
once instead of after a timeout. Every gateway node serves it over RPC on
TCP, through the endpoint mapper, for continuously available shares
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)):

- `WitnessrGetInterfaceList` lists every gateway node's floating addresses,
  each marked available or not;
- `WitnessrRegister` / `WitnessrRegisterEx` record the client, net name, share
  and address in the memory of the node registered with, which the client picks
  to be a different node from its SMB one;
- `WitnessrAsyncNotify` sends `RESOURCE_CHANGE` when an address moves,
  `CLIENT_MOVE` naming the target node when the client's node drains, and
  `IP_CHANGE` when a node's address list changes. `SHARE_MOVE` is not sent.
- Registrations are not stored. A client whose Witness node is lost sees its
  `AsyncNotify` fail and registers again elsewhere.

## 15. Previous Versions

Explorer's Previous Versions tab lists older copies of a file to open or
restore; here they are the share's read-only snapshots
([RFC 12](rfc-12-snapshots.md)).

- `FSCTL_SRV_ENUMERATE_SNAPSHOTS` returns, newest first, the
  `@GMT-YYYY.MM.DD-HH.MM.SS` token of every complete snapshot **visible at the
  path of the handle it is sent on**: every share snapshot, and every subtree
  snapshot whose root lies on that path's chain of ancestors
  ([RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot),
  [§2.10](rfc-12-snapshots.md#2.10%20Subtree%20snapshots)). Tokens and ordinals
  are unique only per path, so a listing per share could show one token twice.
- A `CREATE` with a timewarp context (`TWrp`), or a path whose first component
  is a token, opens the file as of the snapshot that token names among those
  visible at the opened path, resolved by the same rule.
- An open asking for write access there is `STATUS_ACCESS_DENIED`; a token
  naming no snapshot is `STATUS_OBJECT_NAME_NOT_FOUND`.
- Each snapshot reports its own volume serial, derived from the share and the
  snapshot's ordinal, never the share's, and its files report the snapshot's
  ids: the 64-bit id is the top bit set, the ordinal and `Number`, and the
  128-bit id's high half is a keyed digest of the `ShareID` and the cut
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused),
  [RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot)). A
  client keying its caches or hard-link detection on volume and file id then
  never takes a file in one snapshot for the same file in another, or in the
  live share.

## 16. Translation

The adapter translates and the core decides
([RFC 20 §2.1](rfc-20-adapters.md#2.1%20Two%20questions%20place%20every%20piece%20of%20logic)).
This chapter is every translation the SMB adapter makes, in one order: who is
calling (§16.1), which retry a request is (§16.2), the namespace (§16.3), open
state (§16.4) and the errors that come back (§16.5); then how the translations
are tested (§16.6) and watched (§16.7).

The adapter reaches the core only through `vfs.Service`
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)): `SignIn`, `Spell` and `Find`
for identities, SIDs and names, and the file and state calls for everything
else. It imports no identity, metadata or open-state package. What it must keep
beyond a connection it keeps in the opaque `Adapter` bytes of a record, which
the core stores and never reads (§16.4).

Every table here has one shape: the core's field, call or error first, the SMB
field or status after it, and, where it matters, a column for what is kept in
`Adapter` bytes rather than in a core field. A request reads a row right to
left, a result left to right. Each mapping is a pure function of the request
and, where named, the record's `Adapter` bytes, and each is total: a wire value
maps to one core value or to a named refusal, never to a default.

### 16.1 Identity

Each session becomes RFC 17's neutral `Identity`
([RFC 17 §3.0](rfc-17-vfs.md#3.0%20Value%20types)), which names no mechanism
and no signing or sealing setting:

| `Proof` | `Protection` | `Evidence.Principal` | ← SMB session |
| --- | --- | --- | --- |
| `None` | `None` | zero: RFC 18 sets a guest session up as anonymous | anonymous (null session) or guest |
| `Secret` | `None` | the principal `Service.SignIn` returned | NTLMv2, unsigned |
| `Secret` | `Integrity` | as above | NTLMv2, signing required |
| `Secret` | `Privacy` | as above | NTLMv2, encrypted |
| `Ticket` | `None` | as above | Kerberos, unsigned |
| `Ticket` | `Integrity` | as above | Kerberos, signing required |
| `Ticket` | `Privacy` | as above | Kerberos, encrypted |

The identity also carries the connection's source address, which client rules
are checked against; an SMB session carries no claims.

- **Share policy.** A share whose `MinProof` is `Ticket` refuses an NTLM
  session and admits a Kerberos one
  ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)); a
  refusal is `ErrWrongSecurity` (§16.5). A share's encryption flag is not a
  protection minimum: it is enforced per tree connect, on the wire (§4.1).
- **Keys.** The session key comes back from `SignIn` beside the `Identity`,
  never in it, and stays on the gateway node (§4.1).
- **Names.** SIDs and names are spelled and parsed through `Service.Spell` and
  `Service.Find`; the adapter keeps no map of its own.
- **What a session proved.** A session established by authentication, not as
  guest or anonymous, is `authenticated`
  ([RFC 18 §6.5](rfc-18-identity.md#6.5%20What%20the%20proof%20proved%3A%20authenticated%2C%20admin%2C%20privileged));
  SIDs a ticket's PAC marks `SE_GROUP_USE_FOR_DENY_ONLY` are the identity's
  deny-only principals
  ([RFC 18 §3.4](rfc-18-identity.md#3.4%20The%20PAC)), carried in its
  `Evidence`.
- **The credential's group.** An SMB session carries no group, so a new file
  takes the principal's primary group
  ([RFC 7 §2.4](rfc-7-namespace-metadata.md#2.4%20Attributes%2C%20and%20who%20writes%20them)). An owner or group in a create's security descriptor
  (`SecD`) is applied after that rule, as a change of owner or group in the
  create's transaction.
- **Privilege.** `FILE_OPEN_FOR_BACKUP_INTENT` sets `OpenRequest.Backup`
  (§16.4); whether the caller is privileged is the core's (§11).

### 16.2 Request identity

SMB names its retries by open state, not by a request identity, so the request
ID a forwarded call carries
([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)) is minted
rather than derived:

| Request ID | ← SMB |
| --- | --- |
| tagged SMB: the gateway node's ID and node epoch, the connection, and a number never reused on it | nothing on the wire; never the message ID |

Message IDs are per connection, so two channels of one session can send two
different requests under one (`ClientGuid`, session ID, message ID), and an ID
derived from them would answer the second with the first's result. A request
the client flags as a replay carries a new request ID, and the primary matches
it against the open as MS-SMB2 3.3.5.2.10 does: a create by its `CreateGuid`
(`Durable.Create`), a lock by the open's lock sequence (`Durable.LockSeq`),
and every other operation on an open by `Channel.Seq`, refusing with
`STATUS_FILE_NOT_AVAILABLE` a `WRITE`, `SET_INFO` or `IOCTL` whose channel
sequence is stale, or that replays one still in flight under an earlier
sequence — never holding it — and no other request (§6.5).

### 16.3 Namespace

[RFC 7](rfc-7-namespace-metadata.md) is protocol-neutral: its records, rules and errors name no SMB
field. This section is where SMB's attribute bits, times, ids and security
identifiers meet them; the information classes that carry them
are [Appendix E](#Appendix%20E%20%E2%80%94%20information%20classes), the descriptors §11. Its errors are
§16.5's.

**Attributes.** RFC 7's stored markers are an attribute set it stores and never
interprets ([RFC 7 §2.1](rfc-7-namespace-metadata.md#2.1%20File)); the `FILE_ATTRIBUTE_*` bits (MS-FSCC 2.6) map
onto them and onto the file's type:

| RFC 7 field | SMB |
| --- | --- |
| `Type` `Directory` | `FILE_ATTRIBUTE_DIRECTORY`, reported from the type, never stored |
| `Type` `Symlink` | `FILE_ATTRIBUTE_REPARSE_POINT`, reported from the type (§12.4) |
| marker read-only | `FILE_ATTRIBUTE_READONLY` |
| marker hidden | `FILE_ATTRIBUTE_HIDDEN` |
| marker system | `FILE_ATTRIBUTE_SYSTEM` |
| marker archive | `FILE_ATTRIBUTE_ARCHIVE` |
| marker sparse | `FILE_ATTRIBUTE_SPARSE_FILE`, set by `FSCTL_SET_SPARSE` (§9.3) |
| no marker and not a directory | `FILE_ATTRIBUTE_NORMAL` |
| none | every other bit — `TEMPORARY`, `OFFLINE`, `NOT_CONTENT_INDEXED`, `ENCRYPTED`, `COMPRESSED`, `RECALL_ON_OPEN`, `RECALL_ON_DATA_ACCESS` — is ignored on a set and never reported |
| `Birth`, `Access`, `Modify`, `Change` | `CreationTime`, `LastAccessTime`, `LastWriteTime`, `ChangeTime` |
| `Size`, `Charged` | `EndOfFile`; `AllocationSize`, rounded up to 4096 (§9.3) |
| `Number`, and the wider id | the 64-bit file ids, and the 128-bit `FileIdInformation` id (§9.1) |
| the share's filesystem identity | the volume serial number, derived from the `ShareID` (Appendix E.1) |
| a `Stream` `File` | an alternate data stream, `name:stream:$DATA` (§12.1); its file id is its base file's |

> decision: the bits that report residency (`OFFLINE`, `RECALL_*`) are never
> reported, because residency is not an attribute
> ([RFC 7 §9.3](rfc-7-namespace-metadata.md#9.3%20Residency%20is%20not%20an%20attribute));
> the others are not stored. Store a further marker when a client is shown to
> read back one it set.

- **Read-only.** `FILE_ATTRIBUTE_READONLY` is the read-only marker, which
  refuses writes and deletes for every protocol as
  [RFC 7 §7.9](rfc-7-namespace-metadata.md#7.9%20The%20read-only%20marker%20refuses%20writes%20and%20deletes)
  states. On a file, a `CREATE` asking `FILE_WRITE_DATA` or `FILE_APPEND_DATA`,
  or with an overwrite or supersede disposition, is `STATUS_ACCESS_DENIED`,
  and `MaximalAccess` omits both rights. On a file or a directory, a `CREATE`
  with `FILE_DELETE_ON_CLOSE` and a `SET_INFO` of `FileDispositionInformation`
  or `FileDispositionInformationEx` setting delete are `STATUS_CANNOT_DELETE`;
  `FileDispositionInformationEx` with
  `FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE` through an open holding
  `FILE_WRITE_ATTRIBUTES` is the neutral ignore-read-only option. A rename
  replacing it is `STATUS_ACCESS_DENIED`. Reads, renames of it, EA and
  attribute sets, clearing the flag included, succeed.
- **Times.** In `FileBasicInformation`, 0 leaves a time unchanged and −1 and −2
  suspend and resume it (§9.3); any other value is a client-given time, stored
  as given ([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)). A time set travels on an open, so a client-given
  time needs write attributes in the open's grant, a mode bit's included
  ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs), §6.1). `ChangeTime` is never suspended (§9.3).
- **Short names.** RFC 7 stores none
  ([RFC 7 §3.7](rfc-7-namespace-metadata.md#3.7%20No%20short%20names)): directory classes report the short name empty,
  `FileAlternateNameInformation` is `STATUS_OBJECT_NAME_NOT_FOUND` and a set of
  one is refused ([Appendix E.3](#E.3%20Not%20supported)).
- **Open by id.** `FILE_OPEN_BY_FILE_ID` is `STATUS_NOT_SUPPORTED`: nothing
  resolves a number to a file
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused), Appendix D).
- **Rights.** `GENERIC_*` bits are mapped to entry rights by the file generic
  mapping this RFC owns (§6.1), and an entry's rights are spelled as §11 says.
- **Principals are spelled by SID.** Each well-known principal of
  [RFC 18 §2.1](rfc-18-identity.md#2.1%20Kinds%20and%20well-known%20principals)
  has a fixed SID, and every other principal its own:

  | Principal | SID |
  | --- | --- |
  | everyone | `S-1-1-0` |
  | owner rights | `S-1-3-4` |
  | creator owner, creator group | `S-1-3-0`, `S-1-3-1` |
  | authenticated users | `S-1-5-11` |
  | built-in users | `S-1-5-32-545` |
  | administrators | `S-1-5-32-544` |
  | network | `S-1-5-2` |
  | system | `S-1-5-18` |
  | anonymous | `S-1-5-7` (ANONYMOUS LOGON) |
  | root | `S-1-22-1-0`, the Unix-user SID of uid 0 |
  | `OWNER@`, `GROUP@` | the file's owner's or group's SID, or CREATOR OWNER and CREATOR GROUP (§11) |
  | a user or group | its SID ([RFC 18 §4.4](rfc-18-identity.md#4.4%20SIDs)) |
  | a deleted principal | the null SID `S-1-0-0` |

  A SID in this table maps back to its principal through `Service.Find`. Of the
  well-known SIDs, a PAC or a pass-through reply may carry only everyone,
  authenticated users and network and have them kept; every other well-known SID
  it carries is dropped and counted
  ([RFC 18 §3.4](rfc-18-identity.md#3.4%20The%20PAC)).
- **Names.** The `windows` name rule's reserved characters are those MS-FSCC
  2.1.5.2 forbids in a component
  ([RFC 7 §3.8](rfc-7-namespace-metadata.md#3.8%20Names%20every%20protocol%20of%20a%20share%20can%20use)). The first upcase table RFC 16 §4.6 names is the
  mapping set of an NTFS volume's `$UpCase` table, which is how Windows servers
  and Samba compare names, so a tree copied from an NTFS volume keeps every
  file, `Straße.docx` beside `STRASSE.docx` included.
- **Delete pending as SMB sees it.** A disposition set or
  `FILE_DELETE_ON_CLOSE` sets RFC 14's mark (§13). While it stands, a `CREATE`
  of the file, and a create, link or rename into the directory it marks, is
  `ErrDeletePending`, `STATUS_DELETE_PENDING`, and `FileStandardInformation`
  reports `DeletePending`. The unlink at the last close is RFC 7's ordinary
  unlink, run as the marking open's principal under its grant
  ([RFC 7 §8.2](rfc-7-namespace-metadata.md#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later)). The same mark refuses NFS requests too, as an access
  error ([RFC 21 §11.3](rfc-21-nfs.md#11.3%20Namespace)).

### 16.4 Open state

Open state takes only neutral requests and returns neutral results and errors
([RFC 14 §1.1](rfc-14-open-state.md#1.1%20Non-goals)). Every SMB → neutral
mapping it needs is here.

| RFC 14 field or call | ← SMB | Kept in `Adapter` bytes | Notes |
| --- | --- | --- | --- |
| `Client.Name` | `ClientGuid` | — | indexed with the protocol; one record per machine (§4.4) |
| `Client.Boot` | unused, zero | — | a `ClientGuid` carries no boot instance; a restarted machine reconnects its handles under the same record |
| `Client.Owner`, `Client.Protect` | zero, false | — | principals bind per session and per open (§4.4) |
| `Client.Pending` | never set | — | an SMB client is confirmed in one step, by `NEGOTIATE` |
| `Client.Adapter` | — | empty | nothing SMB-only must outlive a connection at the client level |
| `Open.Mode.Access` | `DesiredAccess`, as granted, generic rights expanded (§6.1) | — | `MAXIMUM_ALLOWED` is resolved to the granted mask before the call |
| `Open.Mode.Deny` | the complement of `ShareAccess` over read, write and delete | — | a share bit set allows that access; RFC 14 holds what is refused |
| `OpenRequest.Backup` | `FILE_OPEN_FOR_BACKUP_INTENT` | — | the service sets `Open.Mode.Privileged` when its caller is privileged (§11, S28) |
| `Open.Durability` | `DHnQ`, `DH2Q`; persistent when `DH2Q` asks it on a continuously available share | — | §6.4 |
| `Durable.Timeout` | the durable timeout the create negotiated | — | MS-SMB2 3.3.5.9.10 bounds |
| `Durable.Create` | `CreateGuid` (`DH2Q`, `DH2C`) | — | zero for a v1 durable open |
| `Durable.Instance` | `AppInstanceId`, `AppInstanceVersion` | — | §6.4 |
| `Durable.LockSeq` | `LockSequenceNumber` and `LockSequenceIndex` | — | §6.5; durable v2 and persistent opens on 3.x |
| `Open.Channel.Seq` | `ChannelSequence` | — | checked at the primary as MS-SMB2 3.3.5.2.10 says (§6.5) |
| `Open.Session` | the session ID with its gateway node and that node's epoch | — | session IDs are unique only per node and process |
| `Open.Via` | the directory the create named | — | zero for a create by file ID, which then grants no delete through a parent |
| `Open.Delete.OnClose` | `FILE_DELETE_ON_CLOSE` | — | `Delete.By` is the creating principal |
| `Opens.Update` (`Delete`) | `FileDispositionInformation`, `FileDispositionInformationEx` | — | through the open |
| `Open.Suspended` | the -1 and -2 timestamp sentinels of a set-info (§9.3) | — | `ChangeTime` is never suspended (S15) |
| `Open.LossSeen` | nothing on the wire | — | `ErrLost` on `FLUSH` is `STATUS_UNEXPECTED_IO_ERROR`, once per open (§9.4) |
| `CachingGrant.Key` | the lease key | — | an oplock has none and covers its one open |
| `CachingGrant.Epoch` | the v2 lease epoch | — | zero for a v1 lease and an oplock |
| `CachingGrant` kind: read; read + write; read + write + handle; any of R, RW, RH, RWH | Level II; exclusive; batch; a lease | the parent lease key and the lease version the reply must echo, written with a persistent open's lease | a break to a level is `RecallGrant` to that kind, acknowledged through `AckRecall` |
| `LockOwner.Name`; `LockOwner.Stack` true | the `OpenID` | — | SMB locks stack and two opens of one client conflict (§7) |
| `Lock` (`LockReq.Ranges`) | every `LOCK` element, in order, in one call | — | all or none; an unlock list stops at the first range not held (MS-SMB2 3.3.5.14.1) |
| `LockReq.Wait` | callback when `SMB2_LOCKFLAG_FAIL_IMMEDIATELY` is clear; none when set | — | only a single-element `LOCK` waits (§7) |
| `LockReq.Replay` | the lock sequence of a durable v2 or persistent open | — | §6.5 |
| `Watch` | `CHANGE_NOTIFY`: completion filter, `WATCH_TREE` | — | §8 |
| every neutral error | its SMB status | — | §16.5 |

**Grants and locks are decided by RFC 14.** How long a break may run before
the grant is downgraded to none, leaving its opens open, is
[RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)'s
(§6.3, S12); that a stacking owner's locks never merge and an exclusive one
conflicts with its own open's is
[RFC 14 §2.3](rfc-14-open-state.md#2.3%20Lock)'s. The adapter only names the
owner and sets `Stack`.

**The `Adapter` bytes** start with an adapter-defined version, so a node of
another version refuses what it cannot read rather than misreading it, and
RFC 14 never reads them. Today only a grant's carry anything.

**Topology on the wire.** Clients of a continuously available share learn where
to move through Witness (§14); a by-reference copy across shards is §16.5's
`OtherShard` row.

**What crosses between nodes** is
[RFC 17 §2.1](rfc-17-vfs.md#2.1%20What%20runs%20where)'s: neutral calls and the
opaque `Adapter` bytes. Sessions, channels, credits and keys stay volatile on
the gateway (S2). What a reconnect must find — a persistent open's fields, its
locks and lease — is in RFC 14's records at the file's primary, reached through
the service; the gateway never answers from a copy.

### 16.5 Errors

[RFC 20 §4.5](rfc-20-adapters.md#4.5%20One%20error%20table%20per%20protocol)
requires one total table per protocol; this is SMB's, for every neutral error
the service returns — RFC 7's
([RFC 7 §11.3](rfc-7-namespace-metadata.md#11.3%20Neutral%20errors)), RFC 14's
and RFC 17's ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)).
Whether a request is held pending or answered at once is the adapter's choice,
so the rows say it. "Held pending" means: an interim `STATUS_PENDING`, the request re-run on the
backoff of §6.3, and the final answer by the request's deadline:
`smb_pending_deadline` (60 s) for `ErrDelay` on a break, recall or clone; the freeze's
end plus 35 s, at most `migration.freeze_timeout` plus 35 s, for a quiesced or
frozen share; `smb_pending_cap` for a transient capacity refusal, a stalled
metadata store or a store conflict (`DelayConflict`); grace's end plus 35 s for `ErrGrace`; none for `ErrBlocked`
(§6.3). These bounds are this section's, held to the two inequalities
[RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks) states, and the two settings
[RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)'s.

**The hold follows the delay's cause.** `ErrDelay` arrives as a `Delay`
carrying its `DelayCause` and, for a freeze, its bound in `Until` —
`DelayRecall`, `DelayClone`, `DelayConflict`, `DelayFrozen` (with `Moved` once
the namespace has gone), `DelayCapacity`, `DelayCommit`, `DelayResources`
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)); the
paragraph above gives each cause's hold bound and the rows below the final
status. It is
`errors.Is` still matches it as `ErrDelay`; grace is its own error,
`ErrGrace`. A moved `DelayFrozen` is not held: the moved share's trees end (§6.3). The adapter chooses the row below
from that cause, never from the operation it was answering: a write delayed by
a freeze is held for the freeze's bound, not as capacity, and so never ends
`STATUS_DISK_FULL`. An `ErrDelay` with no cause is held as a break is, up to
`smb_pending_deadline`.

| Service error | SMB answer |
| --- | --- |
| `ErrNotFound` | `STATUS_OBJECT_NAME_NOT_FOUND` for the last component of a path, `STATUS_OBJECT_PATH_NOT_FOUND` for an earlier one |
| `ErrExist` | `STATUS_OBJECT_NAME_COLLISION` |
| `ErrNotEmpty` | `STATUS_DIRECTORY_NOT_EMPTY` |
| `ErrNotDir`, `ErrIsDir` | `STATUS_NOT_A_DIRECTORY`, `STATUS_FILE_IS_A_DIRECTORY` |
| `ErrAccess` | `STATUS_ACCESS_DENIED` |
| `ErrAccess` with cause `ReadOnly` — the read-only marker refused a delete ([RFC 7 §7.9](rfc-7-namespace-metadata.md#7.9%20The%20read-only%20marker%20refuses%20writes%20and%20deletes)) | `STATUS_CANNOT_DELETE` |
| `ErrWrongShare` | `STATUS_ACCESS_DENIED` |
| `ErrStale` — a released file | `STATUS_OBJECT_NAME_NOT_FOUND`: an open holds its file, so only a durable reconnect meets it (§6.4) |
| `ErrCross` (`CrossShare`) | `STATUS_NOT_SAME_DEVICE`; it does not arise from a rename, whose target is resolved within the tree |
| `ErrDelay` — a break or recall in progress, or a clone holding the range | held pending; re-run when it ends. A break that reaches its deadline is forced (§6.3) and the request proceeds with the re-run's own result; one still delayed by a further break or recall at `smb_pending_deadline` is `STATUS_FILE_NOT_AVAILABLE`, the status a held freeze or grace ends with. I/O held by a running clone past its own deadline is `STATUS_FILE_LOCK_CONFLICT`, unless the `Delay` carries `DelayCommit` or `DelayCapacity`, or the call meets `ErrUnavailable` — the clone itself held up by a stalled store, by space it waits for (a clone resumed at start included) or by a remote outage — and the request is then held and answered as that cause's row says, so a clone slowed by a stalled store ends `STATUS_UNEXPECTED_IO_ERROR` at `smb_pending_cap`, and one waiting for space `STATUS_DISK_FULL` there, never a lock conflict at `smb_pending_deadline`. A `CREATE` of a clone's destination not yet served while its clone waits for space is answered the same way, as capacity ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)) |
| `ErrDelay` — a store conflict still unresolved at the service's deadline (`DelayConflict`) | held pending and re-run up to `smb_pending_cap`; still conflicting there, `STATUS_FILE_NOT_AVAILABLE` |
| `ErrDelay` — the share quiesced or frozen for a move ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)) | held pending until the freeze ends, then re-run; at `migration.freeze_timeout` plus 35 s, `STATUS_FILE_NOT_AVAILABLE`; if the namespace has moved, `STATUS_NETWORK_NAME_DELETED` and the moved shares' trees end (§6.3) |
| `ErrDelay` — the journal's capacity or the share's journal limit refusing a write while offload or repack can drain it, a remote outage included ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)) | held pending and re-run for as long as the cause lasts, up to `smb_pending_cap` (10 min by default); `STATUS_DISK_FULL` only at that cap, or at once if the cause turns permanent |
| `ErrDelay` with `DelayCommit` — the metadata store stalled: its own transaction, the existence commit a stable write or `FLUSH` waits on (§9.4; [RFC 8 §11.2](rfc-8-engine.md#11.2%20Every%20condition%20in%20RFC%200%20%C2%A710%20has%20its%20engine%20behaviour%20here)), a raise of the share's version limit, or a write waiting on quota slack | held pending and re-run up to `smb_pending_cap`; only there `STATUS_UNEXPECTED_IO_ERROR`, as a failed read is ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)) |
| `ErrUnavailable` — a read, query or control request meeting a remote outage on a block not held locally ([RFC 8 §7.5](rfc-8-engine.md#7.5%20An%20unreachable%20remote%20fails%20the%20read%2C%20distinguishably)) | held pending and re-run up to `smb_pending_cap`, as a store stall is; only there `STATUS_UNEXPECTED_IO_ERROR`. A `READ` whose verified bytes reach its `MinimumCount` is answered with them at once, short; one whose verified bytes fall below `MinimumCount` is held the same way, never answered short, since MS-SMB2 3.3.5.12 fails a read below `MinimumCount` and the client would see an error for a transient outage. `ErrLost` and `ErrCorrupt` are answered at once ([RFC 17 §5.2](rfc-17-vfs.md#5.2%20Read)) |
| `ErrIncomplete` — a read, through a snapshot or a copy made from one, of a file its cut lists as incomplete ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), or a copychunk or duplicate-extents from such a file, refused before anything changes ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)) | `STATUS_UNEXPECTED_IO_ERROR` at once |
| no service error — the adapter's own refusal of a request past its connection's floor while the node's hold pool cannot take it (§3.3) | `STATUS_INSUFFICIENT_RESOURCES` at once; nothing is held, never `STATUS_DISK_FULL` or `STATUS_FILE_LOCK_CONFLICT` |
| `ErrDelay` — a node memory pool spent: the share's cap on the request buffers, open state, or a reply reservation's deadline (`DelayResources`, [RFC 24 §4.2](rfc-24-resources.md#4.2%20Across%20shares%3A%20the%20share%20cap%20at%20admission)) | `STATUS_INSUFFICIENT_RESOURCES` at once; never held, since holding it would spend the hold pool on the share short of memory |
| `ErrBlocked` — a lock left waiting (§7) | pending, with no deadline; completed on grant, or after `CancelLock` |
| `ErrStale` — the share is being removed or retired ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)) | `STATUS_NETWORK_NAME_DELETED` for every request on a tree connected to it, so the client drops the tree; a new `TREE_CONNECT` to its name, now free, is `STATUS_BAD_NETWORK_NAME`; a durable handle's reconnect is `STATUS_OBJECT_NAME_NOT_FOUND`. A request held pending on the share's state is answered when `Revoked` reports it with the share-removed reason ([RFC 14 §11](rfc-14-open-state.md#11.%20Interface), [RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)): an outstanding `CHANGE_NOTIFY` with `STATUS_NOTIFY_CLEANUP`, a waiting lock or any other with `STATUS_NETWORK_NAME_DELETED`; the waiter is already gone, so no `CancelLock` precedes it |
| `ErrNoSpace` — a permanent capacity refusal, as [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s one table classes it | `STATUS_DISK_FULL` at once |
| `ErrQuota` — a logical quota, its scope named ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `STATUS_DISK_FULL` at once for a share-wide or project quota, the volume the client sees; `STATUS_DISK_QUOTA_EXCEEDED` at once for a user or group one |
| `ErrGrace` — a new open or lock in a shard in grace | held pending until grace ends, then re-run; at grace's end plus 35 s, `STATUS_FILE_NOT_AVAILABLE` |
| `ErrInvalid` — a name RFC 7 refuses; a copy or clone of overlapping ranges of one file, or a duplicate-extents request longer than `clone_max_len` (§10) | `STATUS_OBJECT_NAME_INVALID` for the name; `STATUS_INVALID_PARAMETER` at once otherwise |
| `ErrReadOnly` — a mutation where only reading is admitted ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `STATUS_MEDIA_WRITE_PROTECTED` at once |
| `ErrNameTooLong` — a name past the share's length limit | `STATUS_OBJECT_NAME_INVALID` at once |
| `ErrCross` (`OtherShard`) — **(cluster)** a by-reference copy or clone whose source and destination are in different shards, whatever their primaries, refused at admission ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | copychunk: never reaches the adapter; the service copies the bytes server-side and the adapter reports them written ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)). Duplicate-extents: `STATUS_NOT_SAME_DEVICE` at once, and the client copies |
| `ErrInconsistent` — a store invariant the server's own state breaks ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)); the service raises a health condition | `STATUS_UNEXPECTED_IO_ERROR` at once |
| `ErrCross` (`CrossProject`) — a rename or link across tree quotas ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `STATUS_NOT_SAME_DEVICE` at once; Explorer copies and deletes instead |
| `ErrCross` (`CrossNamespace`) — a copy or clone between shares in different namespaces (§10) | `STATUS_NOT_SUPPORTED` at once; the client copies the bytes itself |
| `ErrLost` on `FLUSH` — a loss of the file's acknowledged writes since this open last flushed (§9.4) | `STATUS_UNEXPECTED_IO_ERROR`, once per open |
| `ErrState` (`NoReclaim`) — a reconnect whose open is gone, an administrator's revoke included ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) | `STATUS_OBJECT_NAME_NOT_FOUND` (§6.4) |
| `ErrState` (`ReclaimConflict`) — a reclaim that conflicts with one already reclaimed in the same grace ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)) | not met: a reconnect reclaims a persistent open its durable record holds, which no other reclaim can hold; were it met, `STATUS_OBJECT_NAME_NOT_FOUND` as for `NoReclaim` |
| `ErrState` (`StaleState`) — a FileId whose open was lost with its shard's open state ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `STATUS_FILE_CLOSED` |
| `ErrState` (`Revoked`) — a FileId whose open an administrator revoked, its client kept ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `STATUS_FILE_CLOSED`: SMB has no revoked status, and the client reopens. Once it has so answered a FileId, the adapter calls `FreeState` for it ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)), as SMB has no `FREE_STATEID`; a later use is `ErrState` (`BadState`) |
| `ErrState` (`BadState`) — a FileId whose open is gone: closed, a removed share's, or never issued ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `STATUS_FILE_CLOSED`; a durable reconnect naming it is `ErrState` (`NoReclaim`), below |
| `ErrState` (`StaleClient`) — the client's record expired, or was marked by a loss of a shard's open state ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)) | `STATUS_FILE_CLOSED` for an open of that client |
| `ErrPerm` — an immutable or append-only file, or a change of owner, group or flags only a privileged caller or the owner may make ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)) | `STATUS_ACCESS_DENIED` |
| `ErrWrongSecurity` — the share's policy does not admit the session's authentication ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `STATUS_ACCESS_DENIED`, at tree connect and on any later request |
| `ErrState` (`NotYours`) | `STATUS_FILE_CLOSED`: the FileId names no open of this session |
| `ErrShareViolation` | `STATUS_SHARING_VIOLATION`; `STATUS_ACCESS_DENIED` for the rename or delete of a directory with an open child (§13) |
| `ErrLocked` | `STATUS_FILE_LOCK_CONFLICT` for I/O; `STATUS_LOCK_NOT_GRANTED` for a lock asked to fail at once |
| `ErrRangeNotLocked` | `STATUS_RANGE_NOT_LOCKED`: an unlock element names a range the open does not hold; the elements before it stay unlocked (§7) |
| `ErrDeletePending` | `STATUS_DELETE_PENDING` |

SMB clients do not retry a request on their own, so every condition that may
clear by itself is held pending, never answered as a failure the client would
show the user. A share's journal limit is a bound on local space that offload
drains, so it is held pending like a full journal; only a logical quota and a
permanent capacity cause do not clear by themselves and are answered at once. Which capacity refusals are
`ErrDelay`, and which become `ErrNoSpace`, is RFC 8 §10.2's one table
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)).

### 16.6 Testing

The translations are tested as RFC 20 §7 says, as pure functions with no
service, before any wire suite runs:

- **Golden vectors per table.** One golden file per table of this chapter —
  sessions (§16.1), request IDs (§16.2), attribute bits, times and ids
  (§16.3), every row of §16.4's table, and every row of §16.5's — asserts
  SMB request → neutral call → SMB reply, byte for byte.
- **Every mapping is total.** Property tests: every SMB input in each decoded
  field maps to a neutral value or a named refusal; every neutral error in
  `vfs.Errors()` has an SMB status (RFC 20 §7.3); access and share masks,
  grant levels and lock elements round-trip.
- **Every decoder is fuzzed.** Native Go fuzz targets (`testing.F`) over
  create-context decoding (`DHnQ`, `DH2Q`, `DH2C`, `RqLs`, `AppInstanceId`),
  access and share masks, `LOCK` elements, security descriptors, and the
  `Adapter`-bytes decoder fed truncated, corrupt, older- and newer-version
  bytes: no panic, and each input either yields a valid neutral request or a
  refusal, never a misread value.

| Requirement | Check | Fails without the rule |
| --- | --- | --- |
| §16.1–§16.5 translation vectors | The golden files and property tests above. | a create context, mask or status left unmapped, or a mapping that differs between two handlers |
| §16.4 decoders fuzzed | The fuzz targets above, each with its seed corpus. | a decoder that panics, or reads another version's bytes as its own |
| §16.4 NLM against an SMB byte-range lock | Hold an NLM lock, take an overlapping SMB byte-range lock: refused; and the reverse. An SMB write into the NLM range is admitted (NFS locks are advisory) while an NFSv3 write into the SMB range is refused. | a lock conflict decided within one protocol only |
| §16.4 SMB deny-write against an NFS write open | Hold an SMB open with `FILE_SHARE_READ` only and no lease; open the file for write over NFSv4 and write it over NFSv3: refused, `NFS4ERR_SHARE_DENIED` and `NFS3ERR_ACCES`; close the SMB open and both succeed. | a deny mode enforced within one protocol only |
| §16.4 persistent state | Fail the primary over under a persistent open with a lease and locks, reconnect: open, lease and locks as left. Open delete-on-close, revoke the client: the open is released and the pending delete runs as its marker. | a reconnect that loses the lease |
| §16.4 end to end | smbtorture and the Windows Protocol Test Suites run through `dt` ([RFC 29](rfc-29-test-harness.md)) as the end-to-end layer above the vectors and fuzz targets. | a translation correct in vectors but wrong against a real client |

### 16.7 Monitoring

The adapter's metrics and request span are
[RFC 20 §10](rfc-20-adapters.md#10.%20Observability)'s, recorded in `core`
under [RFC 25](rfc-25-observability.md)'s conventions; these are their SMB
rows, all with `protocol="smb"`
([RFC 25 §2.4](rfc-25-observability.md#2.4%20Labels)). No share, client,
session, file or principal label.

| Answers, for SMB | Metric | Type |
| --- | --- | --- |
| SMB inputs refused as unmappable, by `reason`: the table that refused it — `identity`, `attribute`, `open`, `create_context`, `lock`, `security_descriptor` | `adapter_translation_refused_total{protocol, reason}` | counter |
| neutral errors spelled as SMB statuses, by the neutral error, from the service's closed error set (§16.5) | `adapter_status_mapped_total{protocol, error}` | counter |
| neutral errors with no row in §16.5; any nonzero is a bug | `adapter_unmapped_errors_total{protocol}` | counter |
| `Adapter` bytes that failed to decode, by `reason` (`truncated`, `corrupt`, `unknown_version`); any nonzero is an alert | `adapter_record_decode_failures_total{protocol, reason}` | counter |
| `CREATE` and `LOCK` replays answered from the record (§6.5) | `adapter_record_replays_total{protocol}` | counter |

**The request span.** One per SMB2 message, named `adapter.smb.<COMMAND>`, with
one child per command of a compound chain and the service's span below it
([RFC 25 §6.1](rfc-25-observability.md#6.1%20Spans)). A request held pending
keeps its span across its re-runs, one child per attempt. Its attributes are
closed sets: protocol, dialect, command, NT status, the neutral error or
result, request and reply bytes, and the stage that refused. Never a path, a
name, a SID, a principal or a client address
([RFC 25 §5.4](rfc-25-observability.md#5.4%20What%20is%20never%20logged)).

## 17. Invariants

| # | Invariant |
| --- | --- |
| S1 | SMB1 and SMB 2.0.2 are never negotiated. |
| S2 | Session keys, channels, credits and tree connects live on one gateway node and are never stored; a session's channels are all on that node. |
| S3 | A FileId's persistent half names an RFC 14 open resolvable on any node. |
| S4 | A durable or persistent reconnect succeeds only for the open's principal, share, `CreateGuid`, `ClientGuid` and lease key. |
| S5 | No worker waits on a break: a conflicting request goes async and the adapter completes it. |
| S6 | Every oplock and lease is an RFC 14 caching grant, visible to NFS. |
| S7 | Timestamp suspension is held with the open at the primary. |
| S8 | A name not valid UTF-8 is never shown through SMB under a substitute spelling. |
| S9 | Every SMB object the adapter maps names a record another RFC owns; the adapter keeps no durable state but the opaque `Adapter` bytes of those records, reached through the service. |
| S10 | A write on a persistent open, or on any open on a continuously available share, is stable before it is acknowledged (S22); a durable open is never reconnected after its primary is lost. |
| S11 | Every session signs or encrypts every request after setup; no share carries its own signing setting. |
| S12 | A break ends before the request waiting on it: the pending deadline exceeds the break deadline. A timed-out break downgrades the grant and leaves its opens open. |
| S13 | A sharing violation against a handle-cached open is decided only after handle caching is broken. |
| S14 | A durable reconnect is routed by the FileId's persistent half; a previous session held on the node is torn down before the new one completes. |
| S15 | No SMB field carries a `FileID`; the SACL is read or changed only by an administrator; `ChangeTime` is never suspended. |
| S16 | A recursive watch reports a change in every shard below its directory, with its detail in the watched shard and as `STATUS_NOTIFY_ENUM_DIR` from another (cluster). |
| S17 | No name with a character MS-FSCC forbids, and no device name, is created from SMB. |
| S18 | Each snapshot reports its own volume serial and file ids, never the live share's or another snapshot's. |
| S19 | A null session reaches no share but `IPC$`; a guest session reaches `IPC$` and only shares whose policy enables guest access and that do not encrypt, decided at `TREE_CONNECT`. |
| S20 | An SMB client record is one `ClientGuid` and holds no principal; every check of who may use an open is against the open's principal. |
| S21 | A waiting lock has no deadline, and is answered other than as granted only after `CancelLock` returns; a request refused `ErrGrace` is held until grace ends plus 35 s. |
| S22 | A stability reply waits for the journal sync of its range and the existence commit of every pending overwrite of committed content in it; a loss of acknowledged writes fails the next `FLUSH` of every open across it, once. |
| S23 | A share's journal limit is held pending like a full journal; a transient capacity refusal is held up to `smb_pending_cap` and answered `STATUS_DISK_FULL` only there; only a logical quota or a permanent cause is answered at once. A request refused for a frozen share is held until the freeze ends plus 35 s. |
| S24 | A copy or clone of overlapping ranges of one file, or a duplicate-extents over `clone_max_len`, is `STATUS_INVALID_PARAMETER` and changes nothing; one across namespaces is `STATUS_NOT_SUPPORTED`; duplicate-extents is atomic, copychunk runs in bounded chunks. |
| S25 | No replay is matched by message ID; a replay never runs beside a request still in flight under an earlier channel sequence; a change notification names a file only as a listing would. |
| S26 | A missed break fences the machine's caching for 90 s, not for the life of its record; at the grant budget no new grant is offered and none held is broken. |
| S27 | A held request's credits are granted in its interim response and its final response grants none; a connection may always hold 256 requests and 32 MiB of writes, more only from the node's hold pool, and a request past its floor with the pool spent is `STATUS_INSUFFICIENT_RESOURCES`. |
| S28 | Privilege applies to a backup- or restore-intent `CREATE` and every request through its handle, never to another handle or to `MaximalAccess`; take-ownership is a right, not a privilege. |
| S29 | A successful durable or persistent reconnect attaches the open to the reconnecting session; an open names its session by node, node epoch and session ID; a moved persistent open's timeout starts at its own client's first tree connect to its share at the new owner. |
| S30 | Times set through an open whose grant holds `FILE_WRITE_ATTRIBUTES` store the client's values; an EA set and a delete-on-close's unlink are decided by the open's grant. |

## 18. Conformance

| Requirement | Check | Fails without the rule |
| --- | --- | --- |
| §3.1 dialects | An SMB1-only and a 2.0.2-only negotiate are refused; a 3.1.1 client gets SHA-512 and its first shared cipher. | an SMB1 fallback left in |
| §4.1 signing | Negotiate from 2.1, 3.0 and 3.1.1 clients; assert `SMB2_NEGOTIATE_SIGNING_REQUIRED` in every response. After session setup, send an unsigned, unencrypted request; assert `STATUS_ACCESS_DENIED`. Set up a null session and a guest one: assert each `TREE_CONNECT` to `IPC$` succeeds and to a data share without the guest flag is `STATUS_ACCESS_DENIED`. Set the share's guest flag: assert the guest's `TREE_CONNECT` succeeds unsigned and the null session's is still `STATUS_ACCESS_DENIED`; set the share to encrypt as well: assert the guest refused. Re-authenticate a signed session as guest: assert it succeeds and its trees remain. Send a signed request with one signature byte flipped; assert `STATUS_ACCESS_DENIED` on its message ID and the next request on the connection served. | a per-share signing flag, unsigned requests accepted, a null session or a guest without the share's flag reaching data, a guest on an encrypting share, or a bad signature dropped unanswered |
| §4.4 client record | From one machine, sign in as alice and then bob on two connections with one `ClientGuid`: assert both sessions succeed under one client record, and that bob's durable reconnect naming alice's FileId is refused. | a principal kept on the SMB client record, or opens checked against it |
| §4.1, §5.1 encryption | A 3.1.1 session that does not encrypt connects to an encrypting share: assert the tree connect reply is unencrypted and carries `SMB2_SHAREFLAG_ENCRYPT_DATA`, the next request is accepted only encrypted, and an unencrypted one is `STATUS_ACCESS_DENIED`. A 3.1.1 client sharing no cipher, and a 2.1 client, are refused at tree connect. | a session refused at tree connect for not encrypting, or an unencrypted request accepted on an encrypting share |
| §4.1 ticket addresses | Sign in with Kerberos tickets carrying no addresses, a NetBIOS address only, the connection's IPv4 address, and another IPv4 address only: assert the first three accepted and the last refused. | addresses ignored, or a NetBIOS entry treated as a restriction |
| §4.2 multichannel | smbtorture `smb2.multichannel`; then bind a channel to a session on another node and assert `STATUS_USER_SESSION_DELETED`. | channels accepted across nodes |
| §4.3, §6.4 reconnect | smbtorture `smb2.durable-open`, `smb2.durable-v2-open`; a Windows client copying a large file while its gateway node is killed finishes the copy; repeat killing the primary. | sessions assumed durable; reconnect matched on FileId alone |
| §6.4 matching | Reconnect with another user, another share, another `CreateGuid`; assert each refused. | |
| §6.4 durable across primary loss | On a share that is not continuously available, open with a durable v2 handle and write unstable; kill the primary before any flush; reconnect. Assert `STATUS_OBJECT_NAME_NOT_FOUND`. Repeat with only the network dropped and the primary up; assert the reconnect succeeds and every write reads back. | a durable open reclaimed over writes the primary lost |
| §6.4, §9.4 stable on persistent | On a continuously available share, open persistent and write 10^4 random 64 KiB pieces without write-through, killing the primary at a random moment; reconnect and assert every acknowledged write reads back. | writes on a persistent open answered before the journal sync |
| §6.4 routing | Open durable, rename the file's directory from another client, drop the network, reconnect naming the old path. Assert the reconnect finds the open. Then reconnect to the same node with `PreviousSessionId` before the node notices the old connection is dead; assert it succeeds. | a reconnect routed by path, or a previous session left holding the open |
| §6.4 app instance | Open a file with an app instance id from client A. Then, with the same id: from client B with read access, assert A's open is closed and B's granted; from client A again (same `ClientGuid`), and from client B as a user without read access, assert A's open survives and the create meets its deny mode as an ordinary create. | takeover from any client, or by a caller who cannot read the file |
| §6.2, §6.3 leases | smbtorture `smb2.lease`, `smb2.oplock`; an NFS open against an SMB RWH lease breaks it and completes. A holder that never acknowledges: assert the waiting `CREATE` completes after the 35 s break deadline and before `smb_pending_deadline`, the holder's lease is none, and its next write through the same open succeeds. | breaks waited on by a worker, a pending request timing out before the break, or opens closed at the break deadline |
| §6.3 handle before sharing | Client A opens with deny-write under an RWH lease and closes its handle locally, so only handle caching keeps it; client B opens for write. Assert A receives a break of handle caching, closes, and B's open succeeds; repeat with A's handle still in use and assert `STATUS_SHARING_VIOLATION` only after the break. | a sharing violation answered before handle caching is broken |
| §16.5 full journal | Fill the journal with offload stalled; send a `WRITE`. Assert `STATUS_PENDING`, then success once offload resumes. Fill one share's journal limit the same way: assert `STATUS_PENDING` and success once offload drains it. Fill the journal with the store unreachable for 2 minutes: assert the `WRITE` stays `STATUS_PENDING` past `smb_pending_deadline` and succeeds once the store returns. Keep it unreachable past `smb_pending_cap`: assert `STATUS_DISK_FULL` only then. Make the store deny puts; assert `STATUS_DISK_FULL` at once. Exceed a user quota; assert `STATUS_DISK_QUOTA_EXCEEDED` at once. Exceed a project quota, then a share-wide one; assert `STATUS_DISK_FULL` at once for each. | a full journal or a share's journal limit answered as a failure at once or at `smb_pending_deadline`, or a quota held pending |
| §6.3, §16.5 freeze | Freeze a share for a move for 2 minutes while a client writes: assert the `WRITE` stays `STATUS_PENDING` past `smb_pending_deadline` and succeeds when the freeze ends. Let the move complete instead, with a waiting lock and an outstanding `CHANGE_NOTIFY` on the share: assert the moved share's tree ends with `STATUS_NETWORK_NAME_DELETED`, the lock `STATUS_NETWORK_NAME_DELETED` and the notify `STATUS_NOTIFY_CLEANUP`, while a tree on another share of the same connection keeps working; a durable handle's reconnect at the new owner is refused and reopened, and a persistent handle reconnects there when the client's first tree connect comes 5 minutes after the new owner began serving. | a write held by a freeze answered `STATUS_DISK_FULL` or failed at `smb_pending_deadline` |
| §6.3 move without grace, on the named address | A namespace moves from A to B naming an address that also carries an SMB client's connection to a moved share; hold the address at A for 5 minutes after B publishes. Assert A closes that connection at its release and accepts no new connection on the address, with no `STATUS_NETWORK_NAME_DELETED`; a second client on another address of A gets `STATUS_NETWORK_NAME_DELETED` for the moved tree. Meanwhile, at B, an SMB `CREATE` of a file nobody held on a moved share succeeds at once, with no `STATUS_PENDING`, and an NFSv4 `OPEN` there is `NFS4ERR_GRACE`. | an SMB request refused by the NFS-only move grace and held for up to an hour, or an SMB client on the named address answered rather than cut off |
| §6.3 move with a Windows client | A Windows client holds a profile disk on a continuously available share through a persistent handle with an RWH lease, deny-write, writing throughout, and a tree on an unmoved share of the same connection. Move the namespace and repoint the name within the client's handle timeout: assert the connection is closed, not answered `STATUS_NETWORK_NAME_DELETED`, the client's reconnect at the new owner reclaims the handle with no application error, and the other tree reconnects. Repeat with the repoint after the client's timeout: assert the handle lost, the application told, and its fresh `CREATE` at the new owner granted once `migration.reconnect_wait` ends the orphaned open. The client is Windows, never a harness that reconnects on cue. | a moved share's tree ended by a status the client does not retry, so a persistent handle dies at A |
| §6.3, §16.5 grace | Fail a primary over; during its grace, a second desktop sends `CREATE` for a file nobody held. Assert `STATUS_PENDING` past `smb_pending_deadline` and success once grace ends; with grace held for its whole period, assert no `STATUS_FILE_NOT_AVAILABLE` before grace's end plus 35 s. | a pending open failed at `smb_pending_deadline` while grace still refuses it |
| §6.3 fence and budget | Let one break time out; assert the client offered no lease for 90 s and offered one after. Drive one client to the grant budget; assert no lease is offered past it, every lease held is kept, and one is offered again once the count falls. | a fence tied to a lease SMB does not have, or the budget breaking a lease in use |
| §3.3 credits under a hold | Hold a connection's writes behind a 9-minute store outage while it keeps sending: assert every interim `STATUS_PENDING` grants credits and every final response grants none, the connection's other tree keeps working, and, with the node's hold pool already filled by a second connection's held writes (its size set through the process memory limit, [RFC 24 §2.3](rfc-24-resources.md#2.3%20The%20pools)), once 256 requests or 32 MiB of writes are held a further write is `STATUS_INSUFFICIENT_RESOURCES` at once and `adapter_hold_refusals_total{protocol="smb"}` rises; after the outage every held write succeeds. | held requests escaping the credit bound, a final response granting credits a second time, or a connection starved of credits while it waits |
| §3.3 multi-session host through a freeze *(1 node)* | One connection carries 40 sessions, each writing its own 4 GiB profile disk at queue depth 8 with 64 KiB writes. Freeze the share for 20 s, then stall the metadata store for 30 s: assert every write held is answered success after each, none `STATUS_INSUFFICIENT_RESOURCES`, `adapter_held_requests` past 256 during the hold, and `adapter_hold_refusals_total{protocol="smb"}` unchanged. | a per-connection cap that fails a multi-session host's writes during a planned freeze or a short stall |
| §16.5 share cap on a profile share *(1 node)* | The same 40-session host, its share the only one with calls in flight, the process memory limit set so its writes take the receive pool past three quarters. Assert no `WRITE` is answered `STATUS_INSUFFICIENT_RESOURCES` and `memory_pool_refusals_total{reason="share_cap"}` is unchanged. | a share cap ([RFC 24 §4.2](rfc-24-resources.md#4.2%20Across%20shares%3A%20the%20share%20cap%20at%20admission)) that fails a lone share's guests with I/O errors while no other share waits for the memory |
| §6.5 replay | smbtorture `smb2.replay`; replay a `CREATE` with the same `CreateGuid` through another node. On a persistent open, send a `LOCK` with a lock sequence, fail the primary over before the reply, and replay it; assert success and one lock held. On two channels of one session, send two different `WRITE`s with the same message ID; assert both applied. Hold a `WRITE` under channel sequence 1 at a clone; on a second channel replay it under sequence 2: assert the replay `STATUS_FILE_NOT_AVAILABLE` while the original is held, a `READ` under 2 served meanwhile, and the replay applied once when sent again after the original ends. Send a `WRITE` and a `READ` under sequence 0 after `Channel.Seq` is 2: assert the `WRITE` refused and the `READ` served. | a lock sequence held only in memory, replays matched by message ID, a replay run beside its original, or a stale `READ` refused |
| §4.3 session after a restart *(1 node)* | Hold a persistent open made on the node's first session after a start; restart the node and, before the client reconnects, set up a session, which takes the same session ID; drop that session's connection: assert the persistent open's timeout does not start, and the client's reconnect then succeeds. | an open named by a session ID a new process reuses |
| §7 locks | smbtorture `smb2.lock`; an SMB lock refuses an NFSv3 write across it. Client A holds an RW lease and a local lock; client B sends `LOCK` over the range: assert A's lease is broken and its lock reaches the server before B is answered. Repeat with A holding an R lease; assert it is broken to none before B is granted. A `LOCK` of three `FAIL_IMMEDIATELY` elements whose third conflicts: assert `STATUS_LOCK_NOT_GRANTED` and none of the three held. An unlock of three ranges whose second is not held: assert `STATUS_RANGE_NOT_LOCKED`, the first unlocked and the third still held. A `LOCK` of two elements without `FAIL_IMMEDIATELY`: assert `STATUS_INVALID_PARAMETER`. | locks held in the adapter, a lock request that does not break read or write caching, or a failed multi-element `LOCK` leaving earlier ranges held |
| §7 waiting locks | Client B waits on client A's lock; A holds it 5 minutes, then releases: assert B's lock granted and its request completed. B waits again; send `CANCEL` racing A's unlock, 10^3 times: assert each request is either `STATUS_CANCELLED` with no lock held by B afterwards, or success with the lock held. | a waiting lock answered on a timer, or a cancel answered before `CancelLock` |
| §6.3 handle before sharing, NFS | Client A opens with deny-write under an RWH lease and closes its handle locally; an NFSv4 client opens for write and an NFSv3 client writes. Assert each is told to retry while A's handle caching is broken, then succeeds. | an NFS request refused by a deny mode only a handle cache held |
| §8 notify | smbtorture `smb2.notify`; overflow returns `STATUS_NOTIFY_ENUM_DIR`; a change made over NFS is reported. With a subtree below the watched directory placed in its own shard, a create in that subtree completes a recursive watch `STATUS_NOTIFY_ENUM_DIR`; create a new shard below the directory after registration and assert a create in it does the same. On a `posix` share, over NFS, create `a:b` and a non-UTF-8 name in the watched directory: assert the first reported as `a` U+F022 `b` and the second not reported. | a recursive watch that sees only its own primary's changes, or a notification naming a file as no listing does |
| §9 info classes | smbtorture `smb2.getinfo`, `smb2.setinfo`, `smb2.dir`, `smb2.streams`; MS-FSA test cases for the classes of Appendix E.1. | |
| §9.3 timestamps | smbtorture `smb2.timestamps`; −1 on one open, write through it and through a second open on another node: only the second moves `LastWriteTime`. −1 on `ChangeTime` too, write through the first open, and assert an NFSv3 `GETATTR` shows `ctime` moved. | suspension in adapter memory, or a frozen NFS ctime |
| §9.3 allocation | Set `FileAllocationInformation` 1 GiB past the end of file; assert success, end of file unchanged and `space_used` over NFS unchanged. | a reservation promised that nothing keeps |
| §9.1 file ids | Query `FileIdInformation`, `FileInternalInformation`, `QFid` and `FSCTL_GET_OBJECT_ID` for a file, and its NFSv3 handle; assert no 8-byte window of the handle's `FileID` appears in any of them. | the `FileID` on the wire |
| §9.4 stability | Append 10^4 write-through writes to a new file with the metadata store stalled: assert every reply arrives. Overwrite committed bytes write-through with the store stalled: assert `STATUS_PENDING` and no success until that write's existence commits. Same with unstable overwrites and `FLUSH`. | an append waiting on a metadata transaction, or an overwrite answered stable before its existence commit |
| §9.4 loss reaches `FLUSH` | Two opens of one file on a share that is not continuously available; write unstable through the first, fail the sync window holding it. Assert the next `FLUSH` on each open is `STATUS_UNEXPECTED_IO_ERROR` and the one after succeeds; drop the network, reconnect the first open durable before flushing, and assert its next `FLUSH` still fails once. Fail several sync windows on installation A so its loss sequence is high, then move a continuously available share holding a persistent open to B and lose one window there: assert the reconnected open's next `FLUSH` at B fails. | a loss reported to no SMB client, or reported on every later flush |
| §10 FSCTLs | smbtorture `smb2.ioctl` (copychunk, sparse, zero data, allocated ranges, duplicate extents). Duplicate extents and copychunk from a file onto an overlapping range of itself: assert `STATUS_INVALID_PARAMETER` and the file unchanged. Write into the source range of a running 512 MiB duplicate-extents: assert an interim `STATUS_PENDING`, the write answered success only after the clone, and the clone equal to the source before the write. Duplicate-extents one byte over `clone_max_len`: assert `STATUS_INVALID_PARAMETER`. Copy a 4 GiB file with copychunk while writing its source: assert no source write waits longer than one chunk. Copy in Explorer between two shares in different namespaces: assert copychunk is `STATUS_NOT_SUPPORTED` and the copy completes by reading and writing. | an overlapping or over-length clone run, a cross-namespace copy attempted, a copy freezing its whole source, or a clone reading a source that changed under it |
| §11 SACL | As an administrator, set a SACL with an audit entry, access the file; assert an access event and that `GETATTR` of `sacl` over NFS, as an administrator, shows it. As the file's owner who is not one, ask for `ACCESS_SYSTEM_SECURITY` over SMB and for `sacl` over NFS; assert `STATUS_PRIVILEGE_NOT_HELD` and `NFS4ERR_ACCESS`. | the SACL open to the file's owner in either protocol |
| §12.3 names | On a `posix` share, create over NFS a non-UTF-8 name and `a:b`; list over SMB. Assert the first absent, the second shown with U+F022 and openable. On a `windows` share, assert both NFS creates refused as invalid names. Create `x?` over SMB: assert `STATUS_OBJECT_NAME_INVALID`. On a `posix` share, create `x` followed by U+F025 over SMB and see `x?` over NFS; on a `windows` share, the same create succeeds and NFS sees the U+F025 code point itself. Create `CON`, `con.txt` and `LPT1` over SMB: assert each `STATUS_OBJECT_NAME_INVALID`; create `CON` over NFS and open it over SMB. | an escape that collides, a forbidden character accepted from SMB, or a device name created from SMB |
| §12.4 symlink targets | Over NFS, create links to `../a/b` and `/srv/x`; query each over SMB. Assert `..\a\b` with `SYMLINK_FLAG_RELATIVE` set and `\srv\x` with it clear. Set a relative target `c\d` over SMB; assert NFS `READLINK` returns `c/d`. | targets shown with `/`, or every target marked absolute |
| §13 rename | smbtorture `smb2.rename`, `smb2.sharemode`; rename a directory with a file below it open over NFS. | |
| §14 Witness | smbtorture `rpc.witness`; drain a node and assert a registered client receives `CLIENT_MOVE` and moves. | |
| §15 snapshots | smbtorture `smb2.twrp`; Explorer's Previous Versions tab lists and restores a file. With subtree snapshots of two disjoint home directories sharing an ordinal, enumerate at each home and at the share root: assert each lists only the snapshots visible at its path, with no token twice, and a `TWrp` open at each home reaches its own subtree's snapshot. Query `FsVolume` and `FileIdInformation` for one file live and through two snapshots; assert three distinct volume serials and three distinct file ids. | a snapshot reporting the share's volume serial, so a client takes its file for the live one |
| §3.1 capabilities | Negotiate 2.1, 3.0, 3.0.2 and 3.1.1, each once with every capability bit set in the request and once with none; assert `Capabilities` equals the §3.1 table's column for each dialect, the "if asked" bits clear when the request left them clear. On a continuously available share, a Windows client asks for a persistent handle. | a continuously available share whose clients ask only for durable handles |
| §4.3 per-session disconnect | Bind two channels of one session; drop one: assert no volatile open closes and no durable timeout starts. On one `ClientGuid`, alice and bob each hold a durable open; alice reconnects with `PreviousSessionId`: assert bob's open stays connected. | a disconnect or previous-session teardown scoped to the machine |
| §6.1 generic rights | Linux `mount.cifs` opens, reads and writes a file mode 0644 owned by its user; assert success. A `CREATE` with `GENERIC_READ` on a file the user may read: granted. As a member of the file's group, not its owner, `mount.cifs` opens, reads and writes a file mode 0664: assert success; set its DOS attributes through the open: assert `STATUS_ACCESS_DENIED`. As a group member, open a file mode 0620 with `GENERIC_WRITE`: assert granted, with `READ_CONTROL` and `SYNCHRONIZE`. | `GENERIC_*` passed through unmapped, a mapped set's minor rights granted to the owner only, or `READ_CONTROL` and `SYNCHRONIZE` granted from the read bit only |
| §16.3 read-only attribute | Set `FILE_ATTRIBUTE_READONLY` on a file. `CREATE` asking `FILE_WRITE_DATA`, asking `FILE_APPEND_DATA`, and with `FILE_OVERWRITE_IF` and `FILE_SUPERSEDE`: assert each `STATUS_ACCESS_DENIED`; with `MAXIMUM_ALLOWED`: assert granted and `MaximalAccess` without either right. `CREATE` with `FILE_DELETE_ON_CLOSE`, and `SET_INFO` `FileDispositionInformation` with delete: assert `STATUS_CANNOT_DELETE`; `FileDispositionInformationEx` with `FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE` through an open holding `FILE_WRITE_ATTRIBUTES`: assert the file goes at close. Rename another file over it with `ReplaceIfExists`: `STATUS_ACCESS_DENIED`. Read it, rename it, set an EA, clear the flag: assert success. On a read-only directory, create a file in it: success; delete it: `STATUS_CANNOT_DELETE`. The same steps against a Windows server give the same statuses. | a read-only file written, truncated or deleted, a delete answered `STATUS_ACCESS_DENIED`, or the flag enforced on a directory's contents |
| §6.1 times through an open | On a 2775 directory, bob creates `report.docx` mode 0664 over NFS. As alice, a member of the group and not the owner, overwrite it with Explorer and with `robocopy /COPY:DT /IS`, then `SET_INFO` a past `LastWriteTime` through alice's open: assert each time she set is stored. Over NFS as alice, `touch -d` a past date on bob's file: assert `NFS4ERR_PERM`; as bob: assert success. | a non-owner writer refused a time set SMB cannot express any other way, or NFS's owner rule relaxed with it |
| §13, §9.5 through the handle | As an `admin`, open with `FILE_OPEN_FOR_BACKUP_INTENT` and `FILE_DELETE_ON_CLOSE` a file whose DACL excludes the `admin`, set an EA through the handle, and close: assert the EA stored and the file gone. Make the deferred unlink fail at the close (the directory made immutable meanwhile): assert the `CLOSE` succeeded, the file stays, and `vfs_deferred_unlink_refusals_total` rose by one. | a delete-on-close or an EA set re-checked without the open's grant, or a refused deferred unlink lost silently |
| §6.1 delete through the parent | In a 0775 directory, a group member deletes and renames another member's file from Explorer, and over NFS; assert both succeed, and both fail in a 0755 directory. | `DELETE` at open decided on the file alone |
| §16.5 delay causes | Hold one write by a freeze, one by a clone, one by a full journal, one by a store stall, each past its hold: assert `STATUS_FILE_NOT_AVAILABLE`, `STATUS_FILE_LOCK_CONFLICT`, `STATUS_DISK_FULL` and `STATUS_UNEXPECTED_IO_ERROR`, never `STATUS_DISK_FULL` for the freeze. Hold a write behind a clone whose own batches meet a store stall: assert it held past `smb_pending_deadline` and ended `STATUS_UNEXPECTED_IO_ERROR` at `smb_pending_cap`, never `STATUS_FILE_LOCK_CONFLICT`. Crash during a clone on a share at its journal limit with the remote down, restart, and write into the source extent: assert it held as capacity, never `STATUS_FILE_LOCK_CONFLICT`. Copychunk from a file a restored snapshot lists as incomplete into an existing file: assert `STATUS_UNEXPECTED_IO_ERROR` at once and the destination readable and unchanged. | a hold chosen by operation rather than cause, or a clone of an incomplete source that poisons its destination |
| §16.5 remote outage on reads | Evict a file's blocks, make the store unreachable for 90 s, read through SMB: assert `STATUS_PENDING` and success once the store returns; past `smb_pending_cap`, `STATUS_UNEXPECTED_IO_ERROR`. Evict only the second half of a file and read all of it with `MinimumCount` equal to its length: assert `STATUS_PENDING`, not a short read or an error; with `MinimumCount` 0, assert the first half answered at once. | a transient outage answered as data loss |
| §16.5 share removal | Remove a share while an SMB client has a tree, a pending `CHANGE_NOTIFY`, a waiting lock and a held write on it: assert `STATUS_NOTIFY_CLEANUP`, `STATUS_NETWORK_NAME_DELETED` for the others, and a tree on another share of the connection unaffected. | `DropShare` never reaching the SMB answer |
| §11 backup intent | A session whose principal a grant names `admin`: open a file its DACL denies without `FILE_OPEN_FOR_BACKUP_INTENT`, assert `STATUS_ACCESS_DENIED` and `MaximalAccess` without the denied right; with it, assert granted. A guest session with the intent: assert denied. As that `admin`, run `robocopy /B /SECFIX` of a tree whose files' DACLs exclude the `admin`, with other owners and groups: assert every file's owner, group and DACL restored. Set another owner through a handle opened without the intent: assert `STATUS_ACCESS_DENIED`. | privilege applied to every open of an `admin` session, or stopping at the intent `CREATE` so a restore cannot set a DACL |
| §5.2 IPC$ | smbtorture `rpc.srvsvc`, `rpc.wkssvc`; a hidden share is absent from `NetShareEnumAll`. | |
| §2 profile containers | The reference workload's sign-in storm and an hour of random 64 KiB overwrites, with a gateway node and then a primary failed mid-run: every container mounts again without a repair prompt, and every write acknowledged before the failure reads back. Then truncate a container by 20% through the session's own handle, as sign-out compaction does, and read it back cold (no cache on the reading node): byte-for-byte equal to the expected prefix. | a truncate through a held handle that leaves stale data past the new end, or loses data before it |
| §4.1 re-authentication | alice holds a Kerberos session with a tree and an open. Re-authenticate with alice's wrong password: the session, its tree and open gone, pending notifies completed `STATUS_NOTIFY_CLEANUP`. Re-authenticate as alice with a new ticket: the tree and open still work and the new expiry is in force. Re-authenticate as bob: a new open is decided for bob, and alice's open keeps the access it was granted. smbtorture `smb2.session.reauth1`–`reauth6` pass as they do against the reference servers. |
| §4.1 expired session | Let a session's ticket expire: assert a `READ` is `STATUS_NETWORK_SESSION_EXPIRED`, and `CLOSE`, an unlocking `LOCK`, a lease break acknowledgement and `LOGOFF` succeed. |
| §4.3 `PreviousSessionId` | alice holds a session; a new `SESSION_SETUP` names it with a wrong password, then as bob: assert alice's session and opens untouched each time. Then as alice: assert it torn down. |
| §5.1 access-based enumeration | On a share with the flag, a directory holding alice's folder and bob's: as bob, `QUERY_DIRECTORY` lists only bob's, and `TREE_CONNECT` reports `SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM`. |
| §11 owner entries | On a mode-0755 file owned by alice, read the descriptor, write it back unchanged, `chown` the file to bob: assert bob, not alice, holds the owner's rights. Write a descriptor naming alice's SID with `OI CI` on a directory: assert it stays a named entry. Over NFSv4 set `deny alice write` and `allow EVERYONE@ write` on alice's file, write the descriptor back unchanged, `chown` to bob: assert alice still refused write. |
| §11 inheritance at set | A set with the auto-inherit request on a child whose parent changed: assert the child's inherited entries recomputed and its explicit ones kept; assert its siblings unchanged. |
| §11 owner SID | Set an owner SID no principal holds together with a DACL: assert `STATUS_INVALID_OWNER` and the DACL unchanged. |
| §11 null DACL | Set a null DACL: assert everyone granted, and one allow-everyone entry read back. |

**Wire tests.** The wire tests of [RFC 20 §8](rfc-20-adapters.md#8.%20Wire%20tests)
run this adapter and a reference SMB server through the same scripts and
compare their replies field by field. This RFC owns the SMB divergence list:
each entry cites the rule here that makes DittoFS differ — SMB1 refused
([§3.1](#3.1%20Dialects)), no 8.3 names ([§9.2](#9.2%20Not%20supported)), no
compression ([§3.2](#3.2%20Algorithms%2C%20and%20what%20is%20not%20offered)).

**Where each check runs.** The protocol suites run against one single node
([index](rfc-index.md#Test%20harness)), the first release's profile
([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)).
Every step that needs a second node — a channel bound at another `protocol`
node (§4.2), a lost gateway node (§4.3), a reconnect or replay through
another node (§6.4, §6.5), a write through a second node (§9.3), and the
profile-container run's gateway node failure (§2) — is **(cluster)** and
outside the first release's conformance; on the single node a restart of
the node stands for the primary's loss.

**What must not stand in.** A single-node run cannot fail §4.2, §4.3, or the
cross-node halves of §6.4 and §6.5. A test client that never lets a break time out cannot fail §6.3. A
failover test that flushes before killing the primary cannot fail §6.4's
durability rules: the writes that matter are the ones not yet flushed. A harness client that reconnects when told
to cannot fail §6.3's move row: the row exists to see what a Windows client does
with the status it is sent.

## 19. Observability

Metric names follow [RFC 25 §2](rfc-25-observability.md#2.%20Metrics) and are
shown without the `dittofs_` prefix the exporter adds.

| Answers | Metric | Type |
| --- | --- | --- |
| sessions and channels per node | `smb_sessions`, `smb_channels` | gauge |
| requests waiting async on a break or lock | `smb_async_pending{reason}` | gauge |
| durable reconnects, by result | `smb_reconnects_total{kind, result}` | counter |
| channel binds refused for another node | `smb_bind_refused_total` | counter |
| names hidden from SMB in listings | `smb_names_hidden_total` | counter |
| Witness registrations per node | `smb_witness_registrations` | gauge |
| requests and bytes held pending, the hold pool in use, requests refused past their floor (§3.3) | RFC 20 §10.1's `adapter_held_requests`, `adapter_held_bytes`, `adapter_hold_pool_bytes`, `adapter_hold_refusals_total{protocol="smb"}` | gauges, counter |
| deferred unlinks refused at the last close (§13) | RFC 17 §7's `vfs_deferred_unlink_refusals_total` | counter |

Operation latency is RFC 17's; leases, breaks and grace are RFC 14's; the
adapter's own rows for SMB, and its request span, are §16.7's. No share or
client label.

## 20. Open questions

1. **Durable opens across a primary failover** are refused
   ([§6.4](#6.4%20Durable%20and%20persistent%20handles)). Whether a per-open
   record of the last synced write is worth its cost, to accept the reconnects
   that lost nothing, is unmeasured.
2. **Opens deeper than direct children**
   ([§13](#13.%20Rename%20and%20delete%20with%20open%20handles)): Windows
   refuses a directory rename when any descendant is open; this server checks
   direct children only
   ([RFC 14 §9.5](rfc-14-open-state.md#9.5%20Open%20children%20refuse%20an%20SMB%20rename%20or%20delete%20of%20their%20directory)).
   Whether an application in the target workload depends on the deeper check is
   unmeasured.
3. **SMB 3.1.1 POSIX extensions** for Linux clients over SMB.
4. **Directory leases**, with NFS directory delegations and RFC 14's open
   question 4.

## Appendix A — prior art

*Non-normative.*

| Server | Choice | Taken or not |
| --- | --- | --- |
| Windows Server | SMB1 off by default; directory leases and ODX on; persistent handles on continuously available shares; Witness on clusters; break timeout 35 s | persistent and Witness taken; ODX and directory leases not |
| Samba | oplocks and leases over one lease table; private-use mapping for reserved characters; durable handles kept in a clustered database; multichannel per node | oplock-as-lease and the character mapping taken; durable records are RFC 14's |
| Clustered NAS with public-address takeover | sessions per node, channels per node, durable reclaim after takeover | taken ([§4.2](#4.2%20Multichannel%20only%20within%20one%20node), [§4.3](#4.3%20Reconnect%20after%20a%20node%20is%20lost)) |

## Appendix B — private-use mapping

*Normative; used by [§12.3](#12.3%20Names%20SMB%20cannot%20carry).*

| Character | Code point |
| --- | --- |
| 0x01–0x1F | U+F001–U+F01F |
| `"` | U+F020 |
| `*` | U+F021 |
| `:` | U+F022 |
| `<` | U+F023 |
| `>` | U+F024 |
| `?` | U+F025 |
| `\` | U+F026 |
| `\|` | U+F027 |
| trailing space | U+F028 |
| trailing `.` | U+F029 |

## Appendix C — flags every share reports

*Normative; used by [§5.1](#5.1%20Share%20flags).*

Every share, whatever its policy:

- reports `SMB2_SHAREFLAG_MANUAL_CACHING` (offline files only when the user
  asks);
- reports `SMB2_SHAREFLAG_ENABLE_HASH_V2` off;
- reports `SMB2_SHARE_CAP_DFS`, `SCALEOUT` and `ASYMMETRIC` clear.

## Appendix D — create options and contexts

*Normative; used by [§6.1](#6.1%20Opening%20a%20file).*

| Option or context | Maps to |
| --- | --- |
| `FILE_DELETE_ON_CLOSE` | pending delete ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)) |
| `FILE_WRITE_THROUGH` | every write on the open is stable ([§9.4](#9.4%20Writes%2C%20flushes%20and%20stability)) |
| `FILE_OPEN_REPARSE_POINT` | the symlink itself, not its target |
| `FILE_OPEN_FOR_BACKUP_INTENT` | the open, and every request through it, is privileged when the session's principal is ([§11](#11.%20Security%20descriptors)); otherwise ignored |
| `GENERIC_*` in `DesiredAccess` | mapped to `FILE_GENERIC_*` / `FILE_ALL_ACCESS` before `Open` ([§6.1](#6.1%20Opening%20a%20file)) |
| `FILE_OPEN_BY_FILE_ID` | `STATUS_NOT_SUPPORTED`: no index from a numeric id to a file ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)) |
| `MxAc` (maximal access) | `Access` |
| `QFid` (on-disk id) | `FileId` as `FileIdInformation` reports it ([Appendix E.1](#E.1%20Supported)) |
| `RqLs` v1 and v2 (lease request) | a lease request ([§6.2](#6.2%20Oplocks%20are%20leases)) |
| `DHnQ`, `DH2Q`, `DHnC`, `DH2C` (durable request, reconnect) | durability and reconnect ([§6.4](#6.4%20Durable%20and%20persistent%20handles)) |
| `AlSi` (allocation size) | allocation size ([§9.3](#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)) |
| `SecD` (security descriptor) | the new file's ACL, owner and group, in place of the inherited ACL ([§11](#11.%20Security%20descriptors)) |
| `ExtA` (extended attributes) | initial extended attributes ([§9.5](#9.5%20Extended%20attributes)) |
| `TWrp` (timewarp) | the snapshot at that time ([§15](#15.%20Previous%20Versions)) |
| `AppInstanceId`, `AppInstanceVersion` | RFC 14's `Durable.Instance`; a second open with the same id, from another client that may read the file, closes the first (§6.4) |
| `SVHDX_OPEN_DEVICE_CONTEXT` (shared virtual disk) | `STATUS_NOT_SUPPORTED` |

## Appendix E — information classes

*Normative; used by [§9](#9.%20File%20information).*

### E.1 Supported

| Query | Classes |
| --- | --- |
| file | `Basic`, `Standard`, `Internal` (RFC 7's numeric id), `Ea`, `Access`, `Position`, `Mode`, `Alignment`, `All`, `Stream`, `Compression` (always none), `NetworkOpen`, `AttributeTag`, `FullEa`, `Id` (volume serial and the derived 128-bit file id, never the `FileID`: [§9.1](#9.1%20What%20is%20reported)), `NormalizedName` |
| directory | `Directory`, `FullDirectory`, `BothDirectory` and `IdBothDirectory` (short name empty), `IdFullDirectory`, `Names`, `IdExtdDirectory` |
| file system | `FsVolume` (serial and object id from `ShareID`, or for a snapshot from the share and its ordinal (§15); label the share name), `FsSize`, `FsFullSize` (the caller's quota where one applies), `FsDevice`, `FsAttribute` (E.2), `FsControl` (quota), `FsObjectId`, `FsSectorSize` (4096) |
| security | owner, group, DACL, SACL ([§11](#11.%20Security%20descriptors)) |
| quota | `SMB2_0_INFO_QUOTA`: per-user quotas, by SID through [RFC 18 §4.4](rfc-18-identity.md#4.4%20SIDs) |

| Set | Classes |
| --- | --- |
| file | `Basic` ([§9.3](#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)), `Rename` ([§13](#13.%20Rename%20and%20delete%20with%20open%20handles)), `Link`, `Disposition` and `DispositionEx` (pending delete, [RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)), `EndOfFile`, `Allocation` ([§9.3](#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)), `Position`, `Mode`, `FullEa` |

### E.2 Volume attributes

`FsAttribute` reports `CASE_PRESERVED_NAMES`, `UNICODE_ON_DISK`,
`PERSISTENT_ACLS`, `NAMED_STREAMS`, `SPARSE_FILES`, `SUPPORTS_HARD_LINKS`,
`SUPPORTS_REPARSE_POINTS`, `SUPPORTS_OBJECT_IDS`, `SUPPORTS_EXTENDED_ATTRIBUTES`
and `SUPPORTS_BLOCK_REFCOUNTING`; `CASE_SENSITIVE_SEARCH` only on a
case-sensitive share ([RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case));
maximum component length 255; and the file system name `NTFS`
([§9.1](#9.1%20What%20is%20reported)).

### E.3 Not supported

| Class | Answer | Why |
| --- | --- | --- |
| `FileAlternateNameInformation` | `STATUS_OBJECT_NAME_NOT_FOUND`, what a volume with 8.3 names off returns | 8.3 names are not generated ([RFC 7 §3.7](rfc-7-namespace-metadata.md#3.7%20No%20short%20names)) |
| `FileShortNameInformation` (set) | `STATUS_NOT_SUPPORTED` | as above |
| `FileValidDataLengthInformation` (set) | `STATUS_NOT_SUPPORTED` | no valid-data length is kept; unwritten ranges read zeros |
| `FileFsLabelInformation` (set) | `STATUS_ACCESS_DENIED` | the label is the share name, set by the management API |
| `FileObjectIdInformation` (set) | `STATUS_NOT_SUPPORTED` | object ids are derived ([Appendix F](#Appendix%20F%20%E2%80%94%20control%20codes)) |
| mandatory label (`LABEL_SECURITY_INFORMATION`) | queried empty; set refused `STATUS_NOT_SUPPORTED` | no record holds it |

## Appendix F — control codes

*Normative; used by [§10](#10.%20Control%20codes).*

| FSCTL | Maps to | Notes |
| --- | --- | --- |
| `SET_ZERO_DATA` | `Deallocate` | sparse or not |
| `QUERY_ALLOCATED_RANGES` | `Seek` | data ranges, by `SEEK_DATA` / `SEEK_HOLE`; a zero ref under a newer overwrite record is data ([RFC 17 §5.2](rfc-17-vfs.md#5.2%20Read)) |
| `SRV_REQUEST_RESUME_KEY` | the open | the key names the open in this node's table |
| `SRV_COPYCHUNK`, `SRV_COPYCHUNK_WRITE` | one `Copy`, the request's chunks as its `Extents`, not atomic | 256 chunks, 1 MiB each, 16 MiB per request; may report fewer bytes written than asked; a key from another node is `STATUS_OBJECT_NAME_NOT_FOUND`; across namespaces `STATUS_NOT_SUPPORTED` ([§10](#10.%20Control%20codes)) |
| `DUPLICATE_EXTENTS_TO_FILE` (and `_EX`) | `Clone`, one atomic clone | any alignment; over `clone_max_len` or overlapping ranges of one file `STATUS_INVALID_PARAMETER`; across namespaces `STATUS_NOT_SUPPORTED` ([§10](#10.%20Control%20codes)) |
| `GET_REPARSE_POINT`, `SET_REPARSE_POINT`, `DELETE_REPARSE_POINT` | symlinks and junctions ([§12.4](#12.4%20Reparse%20points)) | other tags refused ([RFC 7 §2.1](rfc-7-namespace-metadata.md#2.1%20File)) |
| `CREATE_OR_GET_OBJECT_ID`, `GET_OBJECT_ID` | the derived 128-bit file id ([§9.1](#9.1%20What%20is%20reported)), never the `FileID` | read-only; set and delete refused |
| `SET_SPARSE` | the sparse attribute | [§9.3](#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files) |
| `SRV_ENUMERATE_SNAPSHOTS` | snapshots | [§15](#15.%20Previous%20Versions) |
| `QUERY_NETWORK_INTERFACE_INFO` | this node's addresses | [§4.2](#4.2%20Multichannel%20only%20within%20one%20node) |
| `VALIDATE_NEGOTIATE_INFO` | the connection | [§3.1](#3.1%20Dialects) |
| `PIPE_TRANSCEIVE`, `PIPE_WAIT`, `PIPE_PEEK` | IPC$ | [§5.2](#5.2%20IPC%24%20and%20the%20RPC%20services) |
| `OFFLOAD_READ`, `OFFLOAD_WRITE` (ODX) | `STATUS_NOT_SUPPORTED` | [§10](#10.%20Control%20codes) |
| `LMR_REQUEST_RESILIENCY`, `SET_COMPRESSION`, `FILE_LEVEL_TRIM`, `GET/SET_INTEGRITY_INFORMATION`, `SVHDX_*` | `STATUS_NOT_SUPPORTED` | |

## Appendix G — commands and the service calls they make

Every command and RPC the SMB adapter serves, the [RFC 17](rfc-17-vfs.md)
service method it becomes, and what the adapter answers without the service.
What each method then calls below is
[RFC 17 §3.5](rfc-17-vfs.md#3.5%20What%20each%20method%20calls)'s.

**SMB 2 and 3.**

| Command | Method | Notes |
| --- | --- | --- |
| `NEGOTIATE` | none (adapter only) | the `ClientGuid` is kept with the connection; the client is registered at its first authenticated `SESSION_SETUP` ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)) |
| `SESSION_SETUP` | RFC 18's authenticator, then `Connect` at the first one that completes for the connection's `ClientGuid` | |
| `LOGOFF` | `Disconnect` | |
| `TREE_CONNECT` | `Root` with the name; `\IPC$`: none | `IPC$` is the adapter's RPC endpoint, not a share, and reaches no file |
| `TREE_DISCONNECT` | none (adapter only) | |
| `CREATE` | `Open` | durable and reconnect contexts (`DHnQ`, `DH2Q`, `DHnC`, `DH2C`), `RqLs`, `MxAc`, `QFid`, `SecD`, `ExtA`, `AlSi`, `TWrp` and `AppInstanceId` are `OpenRequest` fields ([Appendix D](#Appendix%20D%20%E2%80%94%20create%20options%20and%20contexts)); `FILE_OPEN_BY_FILE_ID` and shared virtual disks not supported |
| `CLOSE` | `Close` | |
| `FLUSH` | `Commit` | |
| `READ`, `WRITE` | `Read`, `Write` | |
| `LOCK` | `Lock` or `Unlock`, every element in one call | ([RFC 17 §2.2](rfc-17-vfs.md#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| `IOCTL` `SRV_COPYCHUNK`, `_WRITE` | `Copy` | `SRV_REQUEST_RESUME_KEY`: none (adapter only) |
| `IOCTL` `DUPLICATE_EXTENTS_TO_FILE` | `Clone` | |
| `IOCTL` `SET_ZERO_DATA` | `Deallocate` | |
| `IOCTL` `QUERY_ALLOCATED_RANGES` | `Seek` | |
| `IOCTL` `SET_SPARSE` | `SetAttr` | |
| `IOCTL` `GET_REPARSE_POINT` | `GetAttr` | |
| `IOCTL` `SET_REPARSE_POINT`, `DELETE_REPARSE_POINT` | `Create` of a symlink; `Unlink` | gap: RFC 7 has no call that turns an existing empty file into a symlink |
| `IOCTL` `GET_OBJECT_ID`, `CREATE_OR_GET_OBJECT_ID` | `GetAttr` | derived ids |
| `IOCTL` `SRV_ENUMERATE_SNAPSHOTS` | `Snapshots` | |
| `IOCTL` `QUERY_NETWORK_INTERFACE_INFO`, `VALIDATE_NEGOTIATE_INFO`, `PIPE_*` | none (adapter only) | |
| `IOCTL` ODX, integrity, compression, `SVHDX_*`, resiliency | not supported ([Appendix F](#Appendix%20F%20%E2%80%94%20control%20codes)) | |
| `QUERY_DIRECTORY` | `ReadDir` | |
| `CHANGE_NOTIFY` | `Watch` | |
| `QUERY_INFO` file classes | `GetAttr`; `Streams` for `Stream`; `Xattrs` for `Ea`, `FullEa`; `Access` for `Access` | |
| `QUERY_INFO` file system classes | `StatFS` | |
| `QUERY_INFO` security | `GetACL` | |
| `QUERY_INFO` quota | `Quota` | |
| `SET_INFO` `Basic`, `EndOfFile`, `Allocation`, `Mode` | `SetAttr` | timestamp suspension is `AttrChange.Suspend`; an allocation below the end of file is a size change (§9.3) |
| `SET_INFO` `Disposition`, `DispositionEx` | `SetAttr` with `DeleteOnClose` through the open | |
| `SET_INFO` `Rename` | `Rename` with the open as `from.Open` | |
| `SET_INFO` `Link` | `Link` | |
| `SET_INFO` `FullEa` | `SetXattr`, `RemoveXattr` | |
| `SET_INFO` security | `SetACL` | |
| `OPLOCK_BREAK` (lease or oplock acknowledgement) | `AckRecall` | |
| `CANCEL` | `CancelLock` for a waiting lock, `Unwatch` for a notification; otherwise none | |
| `ECHO` | none (adapter only) | |
| srvsvc `NetShareEnumAll` | `ListShares` | |
| srvsvc `NetShareGetInfo` | `ListShares`, filtered by name | |
| srvsvc `NetSrvGetInfo`, wkssvc `NetWkstaGetInfo` | none (adapter only) | |
| lsarpc `LookupSids2/3`, `LookupNames3/4` | none: `vfs.Service`'s `Spell`, `Find` ([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)) | |
