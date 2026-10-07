---
rfc: 22
title: "RFC 22 — SMB"
component: smb
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-21-nfs]]"
aliases:
  - RFC 22
  - SMB
tags:
  - rfc
---
# RFC 22 — SMB

**Status:** draft. [§19](#19.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone changing the SMB adapter — dialects, sessions, opens and
leases, information classes, FSCTLs, the IPC$ services — and anyone deciding
what a Windows client sees when a node goes away.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

**What this is.** SMB (Server Message Block) is the protocol Windows uses for
file shares: the `\\server\share` paths in Explorer. A client signs in, connects
to a share, opens files, and reads, writes and locks through those opens; the
server can call the client back, for instance to take back a caching promise.
This RFC is the SMB adapter: the part of DittoFS that speaks SMB and translates
each request into a call on the filesystem service ([RFC 17](rfc-17-vfs.md)). It
makes every choice the SMB specifications leave to the server — dialects,
credits, how a durable handle is matched on reconnect, how a name made over NFS
looks on Windows — and says which record elsewhere each SMB object stands for.
It owns no durable state of its own: names, file IDs and attributes are
[RFC 7](rfc-7-namespace-metadata.md)'s; opens, leases, locks and durable-handle
identity are [RFC 14](rfc-14-open-state.md)'s; which shares exist and who may
connect to them are [RFC 17](rfc-17-vfs.md)'s. Behind the service, writes land
in a local journal and move to an object store in the background
([RFC 0](rfc-0-data-lifecycle.md)); SMB sees none of that.

**How a client talks to the server, and what goes wrong.** Take the cluster:
`protocol` nodes **P1** and **P2** hold client connections, and storage node
**S1** is the primary for `profiles/alice/` — the one node that orders every
change to those files. alice signs in to **alice-pc**, and Windows attaches her
profile container, `profiles/alice/ODFC_alice.vhdx`: a 30 GB virtual disk with
about 10.5 GB used, held open all day and written in 64 KiB pieces.

1. **Negotiate and sign in.** alice-pc connects to the cluster's floating
   address, held by P1. The two agree a **dialect** (SMB 3.1.1) and the signing
   and encryption algorithms. alice-pc sets up a **session** with alice's
   Kerberos ticket; the session's keys live in P1's memory only.
2. **Tree connect.** alice-pc connects to `\\dittofs\profiles`. Every request
   is already signed, which the server required at negotiate. The reply
   carries the share's flags: whether every request must also be encrypted,
   and whether the share is continuously available.
3. **Open.** alice-pc sends `CREATE` for the VHDX asking for three things:
   read and write access, letting others read but not write; a **lease** with
   read, write and handle caching (RWH), so it may cache reads, buffer writes
   and keep the file open locally; and a **durable handle**, so the open
   outlives a lost connection. S1 records the open and grants all three.
   alice-pc gets a **FileId** in two halves: one derived from S1's record of
   the open, valid on any node, and one naming an entry in P1's local table.
4. **Network blip.** alice-pc's link drops for 10 s and its TCP connection
   with it. The open does not go away: it lives at S1, which is still up, and
   is durable, kept for its timeout — 60 s unless the client asked for more, at
   most 300 s. Every write S1 acknowledged is still in S1's journal.
5. **Reconnect.** alice-pc sets up a new session, tree connects again and sends
   a reconnect `CREATE` naming the old FileId. It succeeds only if the user,
   the share, the client's identifiers and the lease key all match the open.
   They do, and alice-pc has its open back. Without a durable handle the open
   would have ended with the connection, and the disk with it.
6. **A second sign-in.** alice signs in on a second desktop while alice-pc still
   holds the disk. Its `CREATE` conflicts with alice-pc's handle caching. The
   adapter answers `STATUS_PENDING` at once and frees its worker; S1 sends
   alice-pc a lease break. alice-pc acknowledges, still using the file; the
   request is re-run, alice-pc's deny mode refuses it, and the second sign-in
   fails cleanly instead of opening the same disk twice. Had alice-pc not
   answered, at the break's 35 s deadline its lease would have been downgraded
   to no caching; its open, and the disk, stay open.

If S1, not the network, had failed, the durable open would have gone with S1's
memory, and with it any write S1 had acknowledged but not yet synced; SMB has
no way to tell the client to resend those. So the reconnect is refused
(`STATUS_OBJECT_NAME_NOT_FOUND`), as on a Windows server that restarted, and
the client sees the disk fail rather than silently lose writes. Only a
**persistent** handle, offered on a continuously available share, survives the
primary: it is stored, every write through it is synced before it is
acknowledged, and it reconnects with its lease.

```text
 alice-pc (Windows, SMB 3.1.1)      second desktop
     │ \\dittofs\profiles                 │ CREATE ODFC_alice.vhdx
     ▼                                    ▼
 ┌──────────── P1 ────────────┐     ┌──────────── P2 ────────────┐
 │ session keys, credits,     │     │ answers STATUS_PENDING,    │
 │ tree connect, FileId table │     │ re-runs the CREATE once    │
 │ (memory only; rebuilt on   │     │ the break has ended        │
 │  reconnect)                │     │                            │
 └─────────────┬──────────────┘     └─────────────┬──────────────┘
               │  ▲ lease break                   │
               ▼  │                               ▼
 ┌────────────────── S1, primary of profiles/alice/ ─────────────────┐
 │ RFC 14: the open, its deny mode, RWH lease, durable identity      │
 │ RFC 7:  names, file IDs, attributes, streams                      │
 └───────────────────────────────────────────────────────────────────┘
```

**The words you need.**

- **`protocol` node / primary** — a node that holds client connections / the one
  storage node that orders a shard's writes ([RFC 0 glossary](rfc-0-data-lifecycle.md#Glossary)).
- **Session, tree connect** — a signed-in user on one connection / that
  session's attachment to one share; both live in one node's memory and are
  never stored.
- **Open and FileId** — one open of one file, and the number naming it on the
  wire ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)).
- **Lease (or oplock)** — a promise that lets a client cache reads (R), buffer
  writes (W) and keep a closed file open (H); taken back by a **break**
  ([RFC 14 §5.1](rfc-14-open-state.md#5.1%20What%20a%20grant%20is)).
- **Deny mode** — which other opens a granted open forbids, checked once, at
  open ([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)).
- **Durable / persistent handle** — an open that outlives a lost connection for
  a timeout / one that also outlives the loss of the primary.
- **Grace period** — a window after a primary loses state in which only
  reclaims of held state are accepted ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)).

**What this RFC promises.**

- Losing a `protocol` node loses no open state and starts no grace period; the
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

**How the rest is organised.** §2 walks a profile container through one day —
sign-in, steady state, a network blip, node failures, sign-out — and names the
section holding each rule; read it first. §4 (sessions) and §6 (opens, leases,
breaks, durable handles, replay) carry the story above. §3 sets dialects,
algorithms and sizes; §5 share flags and the `IPC$` services. §7 (byte-range
locks), §8 (change notification), §9 (file information), §10 (control codes),
§11 (security descriptors), §12 (names), §13 (rename and delete) and §15
(Previous Versions) can be skimmed on a first read. §14 covers Witness, the
service that tells a client where to reconnect; §16–§18 the invariants,
conformance checks and metrics. The appendices hold the reference tables:
create options, information classes, control codes and the character mapping.

---

## In short

- SMB 2.1, 3.0, 3.0.2 and 3.1.1 are served. SMB1 and SMB 2.0.2 are refused.
  Compression, SMB Direct and QUIC are not offered.
- The adapter only translates. Opens, durable and persistent handle identity,
  leases, locks, pending deletes, watches and copy state are
  [RFC 14](rfc-14-open-state.md)'s; names, streams, FileIds and attributes
  [RFC 7](rfc-7-namespace-metadata.md)'s; share flags [RFC 16](rfc-16-metadata-store.md)'s
  `ExportPolicy.SMB`; tree connect and enumeration [RFC 17 §4.9](rfc-17-vfs.md).
- Sessions, channels, credits and keys live in one `protocol` node's memory.
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
  [RFC 14](rfc-14-open-state.md)'s; FileId derivation and 8.3 short names
  [RFC 7](rfc-7-namespace-metadata.md)'s; share policy [RFC 16](rfc-16-metadata-store.md)'s;
- decide admission, tree connect or share enumeration — [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees);
- decide authentication, principals or the mapping of users to Windows
  security identifiers (SIDs) — RFC 18 (planned); or the mapping between a
  Windows security descriptor and the stored access control list (ACL) — RFC
  19 (planned);
- restate MS-SMB2 framing, or the status-code mapping, which RFC 20 (planned)
  owns with [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values);
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
6. **S1 fails.** Another storage node becomes primary of `profiles/alice/`. The
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
   at the primary ([RFC 14](rfc-14-open-state.md)), and the session ends on
   its `protocol` node.

The conformance check for this walk is the profile-container row of
[§17](#17.%20Conformance).

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

> decision: old dialects are refused. SMB1 has no unbroken signing, no durable
> handles and no leases; SMB 2.0.2 has no leases and no client identifier
> (`ClientGuid`), which is how [RFC 14](rfc-14-open-state.md) knows a client.
> Only clients older than any supported Windows release lose out. Revisit 2.0.2
> only for a client population that cannot negotiate 2.1.

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
> compression would cost the `protocol` node CPU on every read. Offer it when a
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
- Each connection targets **512** credits outstanding. A response grants what
  the client asks, at least 1, up to the target; the server never grants past
  it and never revokes credits already granted.
- Sequence windows, credits and outstanding requests are per connection, in the
  memory of the `protocol` node.

> ponytail: one credit target for every connection. 512 keeps a client's
> pipeline full on a LAN and bounds what one client pins (RFC 24, planned); the
> ceiling is many idle connections each allowed 512. Make it adapt to node
> memory when idle connections, not busy ones, are what exhausts a node.

## 4. Sessions

A session is a user signed in on a connection; it holds the keys that sign and
encrypt every later message. The standard lets one session span several
connections (multichannel). We keep each session, with all its connections, in
one `protocol` node's memory; a client that loses the node signs in again.

### 4.1 Signing in

- `SESSION_SETUP` carries SPNEGO (the negotiation wrapper that picks an
  authentication method) with Kerberos or NTLMv2 (NT LAN Manager version 2).
  NTLMv1 and LM (LAN Manager) are refused. Which identity results, guest and anonymous
  access, and whether NTLM is allowed at all are RFC 18's.
- A Kerberos ticket may list the client addresses it is valid from. Only its
  IPv4 and IPv6 entries restrict which client may use it: a ticket with none
  is accepted from any address, and a ticket with any **MUST** be refused
  unless the connection's address is one of them. Entries of other types,
  such as NetBIOS names, are ignored.
- The session key, signing key, encryption and decryption keys and the
  preauthentication hash are held in the memory of the `protocol` node, and
  **MUST NOT** be stored or sent to another node.
- **Signing is required server-wide, at negotiate.** Every `NEGOTIATE`
  response **MUST** set `SMB2_NEGOTIATE_SIGNING_REQUIRED`, so every client
  signs from session setup on. After a session is set up, every request on it
  **MUST** be signed or encrypted, and one that is neither is
  `STATUS_ACCESS_DENIED`. There is no per-share signing setting: SMB2 cannot
  tell a client that one share needs signing and another does not, so a
  per-share requirement could only refuse an unsigned client's every request on
  that share.
- A share's encryption flag ([§5.1](#5.1%20Share%20flags)) is enforced per
  share connection, not per session: a session that does not encrypt may
  connect to an encrypting share, and every request on that tree is then
  encrypted. Only a client that cannot encrypt at all is refused at tree
  connect.

> decision: a session with no session key — anonymous or guest, if RFC 18
> admits one — is exempt from the signing requirement, because it holds no key
> and cannot sign at all (MS-SMB2 3.3.5.5.3). The exemption is worth only what
> such a session can reach, which RFC 18 decides; withdraw it, refusing such
> sessions outright, if one can ever reach a share holding data.

### 4.2 Multichannel only within one node

Multichannel lets one session use several TCP connections, say one per network
card. A client asks for the server's addresses
(`FSCTL_QUERY_NETWORK_INTERFACE_INFO`) and binds new connections to its session.

- A session's channels **MUST** all be on one `protocol` node.
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

- When a `protocol` node is lost, its sessions, tree connects and the volatile
  halves of FileIds go with it
  ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)).
- The client reaches a surviving node through a floating address or Witness
  ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)), sets up a new
  session, tree connects, and reconnects its durable and persistent opens
  ([§6.4](#6.4%20Durable%20and%20persistent%20handles)).
- **`PreviousSessionId` is honoured on the node that holds it.** A
  `SESSION_SETUP` naming a previous session that this node still holds, for
  the same user, **MUST** tear that session down before it completes, as
  MS-SMB2 3.3.5.5.3 requires: its volatile opens close and its durable and
  persistent opens are marked disconnected at their primaries, so the
  reconnects that follow find them free. This is the common case after a
  network blip, where the client comes back to the same node through the same
  floating address before the node has noticed the old connection is dead. A
  previous session this node does not hold — its node was lost — names nothing
  and is ignored.
- Volatile opens are lost, as after any server failure.
- A lost `protocol` node loses no open state, so no grace period runs.
- A lost primary is a failover. Its persistent opens are stored and reconnect;
  its durable opens are gone, and their reconnects are refused
  ([§6.4](#6.4%20Durable%20and%20persistent%20handles)). New opens wait out
  the shard's grace
  ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)), held
  pending as a break is ([§6.6](#6.6%20Service%20errors%20on%20the%20wire)).

## 5. Shares and IPC$

A tree connect attaches a session to one share, such as `\\dittofs\profiles`;
its reply tells the client whether requests must be encrypted and whether the
share is continuously available (CA: handles survive a server's
loss). `IPC$` is a share with no files, through which Windows calls management
services — listing shares, turning a security identifier into a name — as
remote procedure calls (DCE/RPC) over named pipes. Admission is
[RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s.

### 5.1 Share flags

`TREE_CONNECT` is `Root` ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)).
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
- **Continuously available.** The share is marked continuously available and
  clustered (`SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY`,
  `SMB2_SHARE_CAP_CLUSTER`, 3.0 and later); persistent handles are granted
  ([§6.4](#6.4%20Durable%20and%20persistent%20handles)); Witness is served
  ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)).
- **Hidden.** Nothing on the wire; the share is omitted from enumeration
  ([§5.2](#5.2%20IPC%24%20and%20the%20RPC%20services)).
- The caller's maximal access (`MaximalAccess`) is the service's `Access` on
  the share root.

The flags every share reports whatever its policy are listed in
[Appendix C](#Appendix%20C%20%E2%80%94%20flags%20every%20share%20reports).

### 5.2 IPC$ and the RPC services

`IPC$` is the adapter's own, reaches no file, and serves named pipes for exactly
three services:

| Service | Calls | Answered from |
| --- | --- | --- |
| server service (srvsvc) | `NetShareEnumAll`, `NetShareGetInfo` (levels 0, 1, 2, 501, 1005), `NetSrvGetInfo` | the share list, filtered as [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees) says |
| workstation service (wkssvc) | `NetWkstaGetInfo` (100, 101) | the installation's name |
| local security authority (lsarpc) | `OpenPolicy2`, `LookupSids2/3`, `LookupNames3/4`, `Close` | RFC 18's principals — the security tab cannot show names without it |

Every other call is answered `DCERPC_FAULT_OP_RNG_ERROR`. Witness is served over
RPC on TCP, not on `IPC$` ([§14](#14.%20Witness)).

> ponytail: three services, the subset Explorer, `net view` and the security tab
> call. Any other management tool that needs a fourth fails. Add a service when
> a supported client workflow fails without it, never for completeness.

## 6. Opens and leases

`CREATE` opens (or creates) a file. It names the access wanted, what other
opens may do meanwhile (share access, [RFC 14](rfc-14-open-state.md)'s deny
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
  nothing indexes files by id; a create carrying an app instance id closes an
  earlier open with the same id, from another client and only for a caller
  that may read the file ([§6.4](#6.4%20Durable%20and%20persistent%20handles));
  virtual-disk sharing is refused.
- The FileId's persistent half **MUST** be derived from the RFC 14 open, and
  name the open's shard, so any node finds the open from it and routes a
  reconnect to its primary without the path (§6.4).
- Its volatile half names the entry in the `protocol` node's table and is
  invalid on any other node.

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
  ([RFC 14](rfc-14-open-state.md)).
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

- A break from read to none needs no acknowledgement.
- **Handle caching is broken before a sharing violation.** A `CREATE` that
  would fail on the deny mode of another client's open, where that open is
  covered by a grant with handle caching, **MUST NOT** fail at once: the
  holder's handle caching is broken first, and the deny modes are checked
  again once the break ends (MS-FSA 2.1.5.1.2). A handle-cached open may be
  one its application has already closed and the client is only keeping; the
  break makes the client close it, and the create then succeeds. Only an open
  still in use after the break refuses the create.
- A lock request from another client recalls a write-caching grant on the
  file before the lock is decided (MS-FSA 2.1.5.7;
  [RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols)), as
  an NFS lock does ([RFC 21 §5.3](rfc-21-nfs.md#5.3%20Delegations)).
- **The break deadline is 35 s**, MS-SMB2's value, and is RFC 14's recall
  deadline for SMB grants ([RFC 14](rfc-14-open-state.md)).
- **A request held pending outlives the break.** The adapter gives every
  request it holds pending — a `CREATE` waiting on a break, a lock waiting on
  a conflict, a write waiting on a full journal — a deadline of **60 s** from
  its arrival, set by the adapter as the call's deadline
  ([RFC 17](rfc-17-vfs.md)). That is longer than the break deadline, so a
  waiting `CREATE` always sees the break end and is re-run, rather than failing
  at RFC 17's 30 s default while the holder still has 5 s to answer.
- **A break not acknowledged by its deadline downgrades; it does not close.**
  At the deadline the lease or oplock is set to none (MS-SMB2 3.3.6.1,
  3.3.6.5), and the waiting request is re-run. The holder's opens stay open: its deny modes and locks still
  apply, and its later reads and writes through them are served as ordinary
  I/O on an open that holds no caching. The client is fenced from caching: it
  is offered no new lease or oplock for the rest of its client lease
  ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).

> decision: a timed-out break downgrades the grant and keeps the opens, as
> Windows servers do. Closing the holder's opens would end a stalled virtual
> machine's profile disk mid-session — the worst outcome for the reference
> workload — to protect against writes the holder buffered under write
> caching arriving after the waiting client's. Those writes still arrive
> through a valid open, and are ordered after the other client's like any two
> uncoordinated writers ([RFC 14](rfc-14-open-state.md)).
> RFC 14 §5.3 requires invalidating the opens; this narrows it for SMB, where
> the open, not the lease, carries the I/O. Revisit if a workload shows stale
> buffered writes overwriting another client's data after a break timeout.

> ponytail: the waiting node learns a break has ended by re-running the request
> on a backoff (10 ms doubling to 1 s), because the acknowledgement may arrive
> at another node; the cost is up to a second of extra wait. Have the primary
> notify the waiter when those retries show up in `CREATE` latency.

### 6.4 Durable and persistent handles

A durable handle survives a lost connection for a timeout: a user on flaky
Wi-Fi keeps their open documents, and a mounted virtual disk does not vanish
under the desktop. It is held in the primary's memory, and does not survive the
primary. A persistent handle is also stored, so it survives the primary
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

**After a primary is lost**, the durable (not persistent) open was held in
memory and is gone, and the reconnect **MUST** be answered
`STATUS_OBJECT_NAME_NOT_FOUND`, as MS-SMB2 answers a reconnect to a server that
restarted. It **MUST NOT** be accepted as a reclaim. A persistent open is a
stored record and reconnects with its lease
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)).

**Writes on a persistent open, or on any open on a continuously available
share, are stable.** Each **MUST** be synced to the primary's journal before
it is acknowledged, whatever its write-through flag (§9.4). SMB has no write
verifier: a client is never told that writes it was answered for were lost,
and never resends them. An open that survives the primary and reconnects with
its lease after the primary lost unsynced writes would let the client go on as
if those writes were on disk — a silent hole in a virtual disk that shows up as
corruption days later. Making writes on surviving opens stable, and refusing
durable opens that do not survive, removes both halves of that.

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
> sync per write on those shares, never a metadata transaction (§9.4). Revisit
> if a measured continuously available workload is limited by sync latency;
> the answer then is a faster journal device, not unstable writes.

Example: after a 10 s blip alice-pc reconnects within its 60 s timeout; a
reconnect by bob's session naming alice's FileId is refused, because the
principal differs.

**App instance takeover.** A `CREATE` carrying an app instance id
(`AppInstanceId`) that an open of the same file already holds closes that open
first ([RFC 14 §8.1](rfc-14-open-state.md) holds the record rule, including
the version check). The adapter **MUST** apply it only when both hold:

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
> RFC 14 states the rule "from any client"; this narrows it at the adapter,
> where `ClientGuid` and maximal access are known. Revisit if a failover
> cluster is shown re-attaching from the same `ClientGuid`.

### 6.5 Replay

A client that sent a request and lost the reply re-sends it marked as a replay.
The server must answer it without doing it twice.

- A `CREATE` marked as a replay (`SMB2_FLAGS_REPLAY_OPERATION`) whose durable v2
  request names, by `CreateGuid`, an existing open returns that open (MS-SMB2
  3.3.5.9.10), on any node, because the `CreateGuid` is kept with the open.
- Other replays (3.x `ChannelSequence`) are checked against the outstanding
  counts the `protocol` node keeps per open; all channels are on that node
  ([§4.2](#4.2%20Multichannel%20only%20within%20one%20node)).
- A `LOCK` replay uses the open's `LockSequence` buckets
  ([RFC 14](rfc-14-open-state.md)): a request whose bucket holds its sequence
  number succeeds without being applied again. It is honoured on 3.x for durable
  and persistent opens. Resilient handles (`FSCTL_LMR_REQUEST_RESILIENCY`) are
  not offered ([Appendix F](#Appendix%20F%20%E2%80%94%20control%20codes)), so
  2.1 has no lock replay.

### 6.6 Service errors on the wire

RFC 20 (planned) owns the full status table; these are mapped here because
whether a request is held pending or answered at once is the adapter's choice.
"Held pending" means: an interim `STATUS_PENDING`, the request re-run on the
backoff of §6.3, and the final answer by the request's 60 s deadline (§6.3).

| Service error | SMB answer |
| --- | --- |
| `ErrDelay` — a break in progress | held pending; re-run when the break ends |
| `ErrDelay` — the journal full until offload or repack frees space | held pending; if the service then answers `ErrNoSpace`, `STATUS_DISK_FULL` |
| `ErrNoSpace` — the share's limit | `STATUS_DISK_FULL` at once |
| `ErrQuota` | `STATUS_DISK_QUOTA_EXCEEDED` at once |
| `ErrGrace` — a new open or lock in a shard in grace | held pending; at the deadline, `STATUS_FILE_NOT_AVAILABLE` |
| `ErrNoReclaim` — a reconnect whose open is gone | `STATUS_OBJECT_NAME_NOT_FOUND` (§6.4) |
| `ErrStaleClient` | `STATUS_FILE_CLOSED` for an open of the expired client |
| `ErrNotYours` | `STATUS_FILE_CLOSED`: the FileId names no open of this session |
| `ErrShareViolation` | `STATUS_SHARING_VIOLATION` |
| `ErrLocked` | `STATUS_FILE_LOCK_CONFLICT` for I/O; `STATUS_LOCK_NOT_GRANTED` for a lock asked to fail at once |
| `ErrDeletePending` | `STATUS_DELETE_PENDING` |

SMB clients do not retry a request on their own, so every condition that may
clear by itself is held pending, never answered as a failure the client would
show the user. A share limit or quota does not clear by itself and is answered
at once ([RFC 8](rfc-8-engine.md)).

## 7. Byte-range locks

A Windows lock covers a byte range of a file and is mandatory: while it is held,
reads and writes of the range by other opens fail, not only other lock requests.
[RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols) enforces
that against every protocol; this section maps the SMB request.

- `LOCK` elements map onto RFC 14 locks under the open: shared or exclusive, a
  64-bit range.
- A conflicting lock asked to fail at once (`FAIL_IMMEDIATELY`) is
  `STATUS_LOCK_NOT_GRANTED`. Without that flag the request goes async, as a
  break does ([§6.3](#6.3%20Breaks)), and completes on grant, on `CANCEL`
  (`STATUS_CANCELLED`) or on close.
- A read or write across a range another open has locked is
  `STATUS_FILE_LOCK_CONFLICT`.
- A lock request from another client on a file under a write-caching grant —
  an SMB lease or oplock, or an NFS write delegation — recalls the grant first
  (MS-FSA 2.1.5.7; [§6.3](#6.3%20Breaks)), and waits pending while it does.
- Unlocking a range not held is `STATUS_RANGE_NOT_LOCKED`.
- A zero-length lock conflicts with nothing and is held.

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
- Closing the open ends the watch; a request outstanding then completes
  `STATUS_NOTIFY_CLEANUP`.
- **A recursive watch spans shards.** A watch lives at its directory's primary
  ([RFC 14](rfc-14-open-state.md)), and a subtree below the directory may be
  its own shard with another primary. A `WATCH_TREE` request **MUST** register
  a watch at the directory and one at the root of each shard placed below it,
  and deliver changes from all of them as one stream. When a shard is placed
  below the directory, or a placement below it changes, after the request was
  registered — the adapter learns placement from the shard map
  ([RFC 15](rfc-15-topology.md)) — the next request **MUST** complete
  `STATUS_NOTIFY_ENUM_DIR`, and
  the adapter registers again; the client re-lists, as it does after an
  overflow. A change is never silently missed for being in another shard.

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
  ([RFC 7](rfc-7-namespace-metadata.md)).
  Every SMB field that reports a file id — `FileInternalInformation`,
  `FileIdInformation`, the directory classes' ids, the `QFid` create context
  and the object-id control codes — **MUST** carry RFC 7's derived ids
  ([RFC 7](rfc-7-namespace-metadata.md)): the 64-bit `Number`, or the 128-bit
  id formed from a keyed value derived from the share followed by `Number`.
  Neither contains a bit of the `FileID` or can be turned back into one.

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
  ([RFC 14](rfc-14-open-state.md)), not in the adapter, because the primary sets
  `Modify` at the existence commit
  ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)).
- A write through a suspending open **MUST NOT** move `LastWriteTime` or
  `LastAccessTime` while a write through another open of the same file still
  does.
- **The change time is never suspended.** A −1 or −2 in `ChangeTime` is
  accepted and changes nothing: the stored change time, which NFS reports as
  ctime, **MUST** move on every write whatever any SMB open suspended. NFSv3
  clients revalidate their caches by ctime, so a frozen ctime would leave every
  NFS reader of a profile disk serving stale data after SMB writes.
- The suspension lasts as long as the open's record at the primary
  ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)): it survives a reconnect
  after a network drop, and a persistent open's survives the primary.

> decision: suspension is open state, not adapter state. In the adapter it
> could only rewrite times after the commit set them, and a client reading
> through another node would see them change twice. The cost is one field per
> open at the primary; nothing is stored.

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
> reservation, because nothing below the adapter reserves space
> ([RFC 21 §9.3](rfc-21-nfs.md#9.3%20NFSv4.2%20operations) refuses NFS's
> `ALLOCATE` for the same reason). Refusing it would fail every Windows copy,
> which sets the allocation size before writing; accepting it costs only the
> promise that the later writes cannot fail for space, which an application
> meets as `STATUS_DISK_FULL` on the write instead. Make it a reservation when
> a lower RFC defines one charged against the share's limit.

**Sparse files.** `FSCTL_SET_SPARSE` sets the sparse DOS attribute in `Flags`
and nothing else: every file may have holes, sparse or not, and holes are RFC
6's.

### 9.4 Writes, flushes and stability

- `WRITE` is an unstable `Write`, except as below.
- It is a stable `Write` with `SMB2_WRITEFLAG_WRITE_THROUGH`, on an open made
  with `FILE_WRITE_THROUGH`, on a persistent open, and on any open on a
  continuously available share (§6.4).
- A stable write **MUST** be answered once the journal has synced the write's
  record, which carries the data with the size and time it sets, and **MUST
  NOT** wait for the metadata store to commit them; the existence commit
  follows lazily ([RFC 8](rfc-8-engine.md)). The synced record alone makes the
  write recoverable, so a profile disk on a continuously available share pays
  one journal sync per write, not a metadata transaction as well.
- `FLUSH` is `Commit`.
- A write refused because the journal is full is held pending, and answered
  `STATUS_DISK_FULL` only if space cannot be freed by its deadline (§6.6).
- A short read follows [RFC 17 §5.2](rfc-17-vfs.md#5.2%20Read).

### 9.5 Extended attributes

SMB extended attributes (EAs) are RFC 7 xattrs, the same records NFS's xattr
operations reach ([RFC 21 §9.3](rfc-21-nfs.md#9.3%20NFSv4.2%20operations)). SMB
compares EA names without case: a set whose name differs only in case from a
stored name replaces that one.

## 10. Control codes

A file-system control code (FSCTL) is SMB's catch-all for operations beyond
read and write: server-side copy, zeroing a range, symlinks, snapshot lists.

- Server-side copy (`FSCTL_SRV_COPYCHUNK`, and cloning by
  `FSCTL_DUPLICATE_EXTENTS_TO_FILE`) is the service's `Copy`, so a copy made in
  Explorer never crosses the network.
- Zeroing a range is `Deallocate`; asking which ranges hold data is `Seek`.
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
> without one. Offer ODX when a workload shows copychunk limiting a copy that
> ODX would not.

## 11. Security descriptors

A Windows security descriptor holds a file's owner and group, its access
control list (DACL, the discretionary ACL: who may do what), and its system ACL
(SACL: which accesses are audited). Here it is RFC 7's ACL through RFC 19's
mapping.

- Owner and group security identifiers (SIDs) are RFC 18's principals.
- The DACL is the ACL's entries.
- The **SACL is the ACL's audit entries**
  ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)),
  stored with it; reading or writing it needs `ACCESS_SYSTEM_SECURITY` on the
  open, which **MUST** be granted only to an administrator of the share (RFC
  18, planned), never by the file's ACL or to its owner. A request for it by
  anyone else is `STATUS_PRIVILEGE_NOT_HELD`. NFS gates the same entries the
  same way ([RFC 21 §8.1](rfc-21-nfs.md#8.1%20Supported)), so neither protocol
  is a way round the other.
- Audit entries never grant or deny; they select the access events the service
  emits ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)).
- A query returns none of the parts the caller did not ask for.

## 12. Names

A stored name is bytes, shared by NFS and SMB; SMB carries UTF-16 and reserves
some characters. This section says how a stored name looks on Windows.

### 12.1 Streams

A Windows file can carry named alternate data streams beside its main content.

- A path component `name:stream` or `name:stream:$DATA` opens the named stream
  (RFC 7's `Stream` file); `name::$DATA` and `name` open the file.
- Any other type after the second colon is `STATUS_OBJECT_NAME_INVALID`.
- Stream names compare without case on every share.
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
   names is not empty to `Remove` (`STATUS_DIRECTORY_NOT_EMPTY`).

A UTF-16 name with an unpaired surrogate is `STATUS_OBJECT_NAME_INVALID`.

**Names SMB refuses to create.** A name an SMB client sends to create or rename
to **MUST** be refused with `STATUS_OBJECT_NAME_INVALID` when it contains a
character MS-FSCC 2.1.5.2 forbids in a component — a control character or one
of `" * / : < > ? \ |` — or when it is a device name: `CON`, `PRN`, `AUX`,
`NUL`, `COM1`–`COM9` or `LPT1`–`LPT9`, compared without case, alone or
followed by a period and an extension (`con.txt`). Windows clients cannot
open such a name through their own path parsing, so one made over SMB would
be a file its own creator cannot reach. A name holding the private-use code
points of Appendix B is not refused: it is case 2's spelling of a reserved
character, and is stored with that character. A device name made over NFS is
listed and openable over SMB, since refusing it there would hide a file
another protocol made under a valid name.

Example: build01 creates `a:b` over NFS; alice-pc sees it with the colon shown
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
- Other tags are refused ([RFC 7](rfc-7-namespace-metadata.md)).

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
  renamed or deleted: `STATUS_ACCESS_DENIED`
  ([RFC 14 §9.5](rfc-14-open-state.md#9.5%20Open%20children%20refuse%20an%20SMB%20rename%20or%20delete%20of%20their%20directory)).
  Opens deeper in the tree are not checked; RFC 14 names that ceiling and its
  upgrade.
- A delete is a pending delete set by disposition or `FILE_DELETE_ON_CLOSE`,
  and is RFC 14's ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close));
  a later `CREATE` of the name is `STATUS_DELETE_PENDING`.

Example: while alice-pc holds `profiles/alice/ODFC_alice.vhdx` open, renaming
`profiles/alice/` is refused, from either protocol.

## 14. Witness

Witness (MS-SWN) tells a client of a continuously available share which server
addresses are up, and to move before a node is drained, so it reconnects at
once instead of after a timeout. Every `protocol` node serves it over RPC on
TCP, through the endpoint mapper, for continuously available shares
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)):

- `WitnessrGetInterfaceList` lists every `protocol` node's floating addresses,
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

- `FSCTL_SRV_ENUMERATE_SNAPSHOTS` returns the `@GMT-YYYY.MM.DD-HH.MM.SS` token
  of every complete snapshot of the share, newest first
  ([RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot)).
- A `CREATE` with a timewarp context (`TWrp`), or a path whose first component
  is a token, opens the file as of that snapshot.
- An open asking for write access there is `STATUS_ACCESS_DENIED`; a token
  naming no snapshot is `STATUS_OBJECT_NAME_NOT_FOUND`.
- The snapshot keeps the share's volume serial.

## 16. Invariants

| # | Invariant |
| --- | --- |
| S1 | SMB1 and SMB 2.0.2 are never negotiated. |
| S2 | Session keys, channels, credits and tree connects live on one `protocol` node and are never stored; a session's channels are all on that node. |
| S3 | A FileId's persistent half names an RFC 14 open resolvable on any node. |
| S4 | A durable or persistent reconnect succeeds only for the open's principal, share, `CreateGuid`, `ClientGuid` and lease key. |
| S5 | No worker waits on a break: a conflicting request goes async and the adapter completes it. |
| S6 | Every oplock and lease is an RFC 14 caching grant, visible to NFS. |
| S7 | Timestamp suspension is held with the open at the primary. |
| S8 | A name not valid UTF-8 is never shown through SMB under a substitute spelling. |
| S9 | Every SMB object the adapter maps names a record another RFC owns; the adapter keeps no durable state. |
| S10 | A write on a persistent open, or on any open on a continuously available share, is synced before it is acknowledged; a durable open is never reconnected after its primary is lost. |
| S11 | Every session signs or encrypts every request after setup; no share carries its own signing setting. |
| S12 | A break ends before the request waiting on it: the pending deadline exceeds the break deadline. A timed-out break downgrades the grant and leaves its opens open. |
| S13 | A sharing violation against a handle-cached open is decided only after handle caching is broken. |
| S14 | A durable reconnect is routed by the FileId's persistent half; a previous session held on the node is torn down before the new one completes. |
| S15 | No SMB field carries a `FileID`; the SACL is read or changed only by an administrator; `ChangeTime` is never suspended. |
| S16 | A recursive watch sees changes in every shard below its directory, or completes `STATUS_NOTIFY_ENUM_DIR`. |
| S17 | No name with a character MS-FSCC forbids, and no device name, is created from SMB. |

## 17. Conformance

| Requirement | Check | Fails without the rule |
| --- | --- | --- |
| §3.1 dialects | An SMB1-only and a 2.0.2-only negotiate are refused; a 3.1.1 client gets SHA-512 and its first shared cipher. | an SMB1 fallback left in |
| §4.1 signing | Negotiate from 2.1, 3.0 and 3.1.1 clients; assert `SMB2_NEGOTIATE_SIGNING_REQUIRED` in every response. After session setup, send an unsigned, unencrypted request; assert `STATUS_ACCESS_DENIED`. | a per-share signing flag, or unsigned requests accepted |
| §4.1, §5.1 encryption | A 3.1.1 session that does not encrypt connects to an encrypting share: assert the tree connect reply is unencrypted and carries `SMB2_SHAREFLAG_ENCRYPT_DATA`, the next request is accepted only encrypted, and an unencrypted one is `STATUS_ACCESS_DENIED`. A 3.1.1 client sharing no cipher, and a 2.1 client, are refused at tree connect. | a session refused at tree connect for not encrypting, or an unencrypted request accepted on an encrypting share |
| §4.1 ticket addresses | Sign in with Kerberos tickets carrying no addresses, a NetBIOS address only, the connection's IPv4 address, and another IPv4 address only: assert the first three accepted and the last refused. | addresses ignored, or a NetBIOS entry treated as a restriction |
| §4.2 multichannel | smbtorture `smb2.multichannel`; then bind a channel to a session on another node and assert `STATUS_USER_SESSION_DELETED`. | channels accepted across nodes |
| §4.3, §6.4 reconnect | smbtorture `smb2.durable-open`, `smb2.durable-v2-open`; a Windows client copying a large file while its `protocol` node is killed finishes the copy; repeat killing the primary. | sessions assumed durable; reconnect matched on FileId alone |
| §6.4 matching | Reconnect with another user, another share, another `CreateGuid`; assert each refused. | |
| §6.4 durable across primary loss | On a share that is not continuously available, open with a durable v2 handle and write unstable; kill the primary before any flush; reconnect. Assert `STATUS_OBJECT_NAME_NOT_FOUND`. Repeat with only the network dropped and the primary up; assert the reconnect succeeds and every write reads back. | a durable open reclaimed over writes the primary lost |
| §6.4, §9.4 stable on persistent | On a continuously available share, open persistent and write 10^4 random 64 KiB pieces without write-through, killing the primary at a random moment; reconnect and assert every acknowledged write reads back. | writes on a persistent open answered before the journal sync |
| §6.4 routing | Open durable, rename the file's directory from another client, drop the network, reconnect naming the old path. Assert the reconnect finds the open. Then reconnect to the same node with `PreviousSessionId` before the node notices the old connection is dead; assert it succeeds. | a reconnect routed by path, or a previous session left holding the open |
| §6.4 app instance | Open a file with an app instance id from client A. Then, with the same id: from client B with read access, assert A's open is closed and B's granted; from client A again (same `ClientGuid`), and from client B as a user without read access, assert A's open survives and the create meets its deny mode as an ordinary create. | takeover from any client, or by a caller who cannot read the file |
| §6.2, §6.3 leases | smbtorture `smb2.lease`, `smb2.oplock`; an NFS open against an SMB RWH lease breaks it and completes. A holder that never acknowledges: assert the waiting `CREATE` completes after the 35 s break deadline and before its 60 s deadline, the holder's lease is none, and its next write through the same open succeeds. | breaks waited on by a worker, a pending request timing out before the break, or opens closed at the break deadline |
| §6.3 handle before sharing | Client A opens with deny-write under an RWH lease and closes its handle locally, so only handle caching keeps it; client B opens for write. Assert A receives a break of handle caching, closes, and B's open succeeds; repeat with A's handle still in use and assert `STATUS_SHARING_VIOLATION` only after the break. | a sharing violation answered before handle caching is broken |
| §6.6 full journal | Fill the journal with offload stalled; send a `WRITE`. Assert `STATUS_PENDING`, then success once offload resumes; with offload still stalled, `STATUS_DISK_FULL` by the 60 s deadline. Fill a share's limit instead; assert `STATUS_DISK_FULL` at once. | a full journal answered as a failure at once, or a share limit held pending |
| §6.5 replay | smbtorture `smb2.replay`; replay a `CREATE` with the same `CreateGuid` through another node. | |
| §7 locks | smbtorture `smb2.lock`; an SMB lock refuses an NFSv3 write across it. Client A holds an RW lease and a local lock; client B sends `LOCK` over the range: assert A's lease is broken and its lock reaches the server before B is answered. | locks held in the adapter, or a lock request that does not break write caching |
| §8 notify | smbtorture `smb2.notify`; overflow returns `STATUS_NOTIFY_ENUM_DIR`; a change made over NFS is reported. With a subtree below the watched directory placed in its own shard, a recursive watch reports a create in that subtree; placing a new shard below it completes the next request `STATUS_NOTIFY_ENUM_DIR`. | a recursive watch registered at one primary only |
| §9 info classes | smbtorture `smb2.getinfo`, `smb2.setinfo`, `smb2.dir`, `smb2.streams`; MS-FSA test cases for the classes of Appendix E.1. | |
| §9.3 timestamps | smbtorture `smb2.timestamps`; −1 on one open, write through it and through a second open on another node: only the second moves `LastWriteTime`. −1 on `ChangeTime` too, write through the first open, and assert an NFSv3 `GETATTR` shows `ctime` moved. | suspension in adapter memory, or a frozen NFS ctime |
| §9.3 allocation | Set `FileAllocationInformation` 1 GiB past the end of file; assert success, end of file unchanged and `space_used` over NFS unchanged. | a reservation promised that nothing keeps |
| §9.1 file ids | Query `FileIdInformation`, `FileInternalInformation`, `QFid` and `FSCTL_GET_OBJECT_ID` for a file, and its NFSv3 handle; assert no 8-byte window of the handle's `FileID` appears in any of them. | the `FileID` on the wire |
| §10 FSCTLs | smbtorture `smb2.ioctl` (copychunk, sparse, zero data, allocated ranges, duplicate extents). | |
| §11 SACL | As an administrator, set a SACL with an audit entry, access the file; assert an access event and that `GETATTR` of `sacl` over NFS, as an administrator, shows it. As the file's owner who is not one, ask for `ACCESS_SYSTEM_SECURITY` over SMB and for `sacl` over NFS; assert `STATUS_PRIVILEGE_NOT_HELD` and `NFS4ERR_ACCESS`. | the SACL open to the file's owner in either protocol |
| §12.3 names | Create over NFS a non-UTF-8 name and `a:b`; list over SMB. Assert the first absent, the second shown with U+F022 and openable. Create `x?` over SMB: assert `STATUS_OBJECT_NAME_INVALID`. Create `x` followed by U+F025 over SMB and see `x?` over NFS. Create `CON`, `con.txt` and `LPT1` over SMB: assert each `STATUS_OBJECT_NAME_INVALID`; create `CON` over NFS and open it over SMB. | an escape that collides, a forbidden character accepted from SMB, or a device name created from SMB |
| §12.4 symlink targets | Over NFS, create links to `../a/b` and `/srv/x`; query each over SMB. Assert `..\a\b` with `SYMLINK_FLAG_RELATIVE` set and `\srv\x` with it clear. Set a relative target `c\d` over SMB; assert NFS `READLINK` returns `c/d`. | targets shown with `/`, or every target marked absolute |
| §13 rename | smbtorture `smb2.rename`, `smb2.sharemode`; rename a directory with a file below it open over NFS. | |
| §14 Witness | smbtorture `rpc.witness`; drain a node and assert a registered client receives `CLIENT_MOVE` and moves. | |
| §15 snapshots | smbtorture `smb2.twrp`; Explorer's Previous Versions tab lists and restores a file. | |
| §5.2 IPC$ | smbtorture `rpc.srvsvc`, `rpc.wkssvc`; a hidden share is absent from `NetShareEnumAll`. | |
| §2 profile containers | The reference workload's sign-in storm and an hour of random 64 KiB overwrites, with a `protocol` node and then a primary failed mid-run: every container mounts again without a repair prompt, and every write acknowledged before the failure reads back. Then truncate a container by 20% through the session's own handle, as sign-out compaction does, and read it back cold (no cache on the reading node): byte-for-byte equal to the expected prefix. | a truncate through a held handle that leaves stale data past the new end, or loses data before it |

**What must not stand in.** A single-node run cannot fail §4.2, §4.3, §6.4 or
§6.5. A test client that never lets a break time out cannot fail §6.3. A
failover test that flushes before killing the primary cannot fail §6.4's
durability rules: the writes that matter are the ones not yet flushed.

## 18. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| sessions and channels per node | `dittofs_smb_sessions`, `dittofs_smb_channels` | gauge |
| requests waiting async on a break or lock | `dittofs_smb_async_pending{reason}` | gauge |
| durable reconnects, by result | `dittofs_smb_reconnects_total{kind, result}` | counter |
| channel binds refused for another node | `dittofs_smb_bind_refused_total` | counter |
| names hidden from SMB in listings | `dittofs_smb_names_hidden_total` | counter |
| Witness registrations per node | `dittofs_smb_witness_registrations` | gauge |

Operation latency is RFC 17's; leases, breaks and grace are RFC 14's. No share
or client label.

## 19. Open questions

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
| `FILE_OPEN_BY_FILE_ID` | `STATUS_NOT_SUPPORTED`: no index from a numeric id to a file ([RFC 7](rfc-7-namespace-metadata.md)) |
| `MxAc` (maximal access) | `Access` |
| `QFid` (on-disk id) | `FileId` as `FileIdInformation` reports it ([Appendix E.1](#E.1%20Supported)) |
| `RqLs` v1 and v2 (lease request) | a lease request ([§6.2](#6.2%20Oplocks%20are%20leases)) |
| `DHnQ`, `DH2Q`, `DHnC`, `DH2C` (durable request, reconnect) | durability and reconnect ([§6.4](#6.4%20Durable%20and%20persistent%20handles)) |
| `AlSi` (allocation size) | allocation size ([§9.3](#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)) |
| `SecD` (security descriptor) | an initial ACL through RFC 19 |
| `ExtA` (extended attributes) | initial extended attributes ([§9.5](#9.5%20Extended%20attributes)) |
| `TWrp` (timewarp) | the snapshot at that time ([§15](#15.%20Previous%20Versions)) |
| `AppInstanceId`, `AppInstanceVersion` | RFC 14's open identity; a second open with the same id closes the first |
| `SVHDX_OPEN_DEVICE_CONTEXT` (shared virtual disk) | `STATUS_NOT_SUPPORTED` |

## Appendix E — information classes

*Normative; used by [§9](#9.%20File%20information).*

### E.1 Supported

| Query | Classes |
| --- | --- |
| file | `Basic`, `Standard`, `Internal` (RFC 7's numeric id), `Ea`, `Access`, `Position`, `Mode`, `Alignment`, `All`, `Stream`, `Compression` (always none), `NetworkOpen`, `AttributeTag`, `FullEa`, `Id` (volume serial and the derived 128-bit file id, never the `FileID`: [§9.1](#9.1%20What%20is%20reported)), `NormalizedName` |
| directory | `Directory`, `FullDirectory`, `BothDirectory` and `IdBothDirectory` (short name empty), `IdFullDirectory`, `Names`, `IdExtdDirectory` |
| file system | `FsVolume` (serial and object id from `ShareID`, label the share name), `FsSize`, `FsFullSize` (the caller's quota where one applies), `FsDevice`, `FsAttribute` (E.2), `FsControl` (quota), `FsObjectId`, `FsSectorSize` (4096) |
| security | owner, group, DACL, SACL ([§11](#11.%20Security%20descriptors)) |
| quota | `SMB2_0_INFO_QUOTA`: per-user quotas, by SID through RFC 18 |

| Set | Classes |
| --- | --- |
| file | `Basic` ([§9.3](#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)), `Rename` ([§13](#13.%20Rename%20and%20delete%20with%20open%20handles)), `Link`, `Disposition` and `DispositionEx` (pending delete, RFC 14), `EndOfFile`, `Allocation` ([§9.3](#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)), `Position`, `Mode`, `FullEa` |

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
| `FileAlternateNameInformation` | `STATUS_OBJECT_NAME_NOT_FOUND`, what a volume with 8.3 names off returns | 8.3 names are not generated ([RFC 7](rfc-7-namespace-metadata.md)) |
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
| `QUERY_ALLOCATED_RANGES` | `Seek` | data ranges, by `SEEK_DATA` / `SEEK_HOLE` |
| `SRV_REQUEST_RESUME_KEY` | the open | the key names the open in this node's table |
| `SRV_COPYCHUNK`, `SRV_COPYCHUNK_WRITE` | `Copy` | 256 chunks, 1 MiB each, 16 MiB per request; a key from another node is `STATUS_OBJECT_NAME_NOT_FOUND` |
| `DUPLICATE_EXTENTS_TO_FILE` (and `_EX`) | `Copy` as a clone | any alignment |
| `GET_REPARSE_POINT`, `SET_REPARSE_POINT`, `DELETE_REPARSE_POINT` | symlinks and junctions ([§12.4](#12.4%20Reparse%20points)) | other tags refused ([RFC 7](rfc-7-namespace-metadata.md)) |
| `CREATE_OR_GET_OBJECT_ID`, `GET_OBJECT_ID` | the derived 128-bit file id ([§9.1](#9.1%20What%20is%20reported)), never the `FileID` | read-only; set and delete refused |
| `SET_SPARSE` | the sparse attribute | [§9.3](#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files) |
| `SRV_ENUMERATE_SNAPSHOTS` | snapshots | [§15](#15.%20Previous%20Versions) |
| `QUERY_NETWORK_INTERFACE_INFO` | this node's addresses | [§4.2](#4.2%20Multichannel%20only%20within%20one%20node) |
| `VALIDATE_NEGOTIATE_INFO` | the connection | [§3.1](#3.1%20Dialects) |
| `PIPE_TRANSCEIVE`, `PIPE_WAIT`, `PIPE_PEEK` | IPC$ | [§5.2](#5.2%20IPC%24%20and%20the%20RPC%20services) |
| `OFFLOAD_READ`, `OFFLOAD_WRITE` (ODX) | `STATUS_NOT_SUPPORTED` | [§10](#10.%20Control%20codes) |
| `LMR_REQUEST_RESILIENCY`, `SET_COMPRESSION`, `FILE_LEVEL_TRIM`, `GET/SET_INTEGRITY_INFORMATION`, `SVHDX_*` | `STATUS_NOT_SUPPORTED` | |
