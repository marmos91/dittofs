---
rfc: 22
title: "RFC 22 — SMB"
component: smb
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
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
- A conflicting request waits on a lease break as an async `STATUS_PENDING`,
  completed by the adapter, never holding a worker.
- Names that are not valid UTF-8 are hidden from SMB; characters Windows cannot
  carry are shown through a fixed private-use mapping. 8.3 names are not
  generated. Timestamp suspension (−1 / −2) is held with the open at the
  primary.

---

## 1. Purpose

MS-SMB2 and MS-FSA fix the wire and the file-system semantics a Windows client
expects, and leave the server a long list of choices: which dialects and
algorithms, how many credits, which information classes and FSCTLs exist, how a
durable handle is matched on reconnect, what happens to a name the other
protocol made. This document makes each once, states why, and maps each SMB
object onto the record that owns it elsewhere in the set.

It answers:

> **For every choice the SMB specifications leave open, what does this server do,
> and which record in the rest of the set does each wire object stand for?**

### 1.1 Non-goals

This document **MUST NOT**:

- define a record. Opens, durable and persistent identity (CreateGuid,
  AppInstanceId, durable timeout, LockSequence), lease keys, parent lease keys
  and epochs, locks, pending deletes and copy state are
  [RFC 14](rfc-14-open-state.md)'s; FileId derivation and 8.3 names
  [RFC 7](rfc-7-namespace-metadata.md)'s; share policy [RFC 16](rfc-16-metadata-store.md)'s;
- decide admission, tree connect or share enumeration — [RFC 17 §4.9](rfc-17-vfs.md);
- decide authentication, principals or SID mapping — RFC 18 (planned); or the
  security descriptor ↔ ACL mapping — RFC 19 (planned);
- restate MS-SMB2 framing, or the status-code mapping, which RFC 20 (planned)
  owns with [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values);
- answer DFS referrals ([RFC 17 §1.1](rfc-17-vfs.md#1.1%20Non-goals)).

## 2. Dialects and negotiation

### 2.1 Dialects

The server negotiates the highest of **3.1.1, 3.0.2, 3.0, 2.1** the client offers.

- **SMB1 is refused.** An SMB1 `NEGOTIATE` naming an SMB2 dialect string is
  answered with the SMB2 multi-protocol response (MS-SMB2 3.3.5.3.1); one
  naming none is answered by closing the connection.
- **SMB 2.0.2 is refused**: a client offering only it is answered
  `STATUS_NOT_SUPPORTED`.
- `FSCTL_VALIDATE_NEGOTIATE_INFO` is answered on 3.0 and 3.0.2 connections.

> decision: SMB1 is refused, because it has no signing that is not broken, no
> durable handles and no leases, and every supported client speaks SMB2. SMB
> 2.0.2 is refused because it carries no `ClientGuid`, which is the client's
> identity in [RFC 14](rfc-14-open-state.md), and no leases. Revisit 2.0.2 only
> for a client population that cannot negotiate 2.1, which no supported
> Windows release is.

### 2.2 Negotiate contexts (3.1.1)

| Context | Offered | Order of preference |
| --- | --- | --- |
| preauthentication integrity | yes | SHA-512 (the only one defined) |
| encryption | yes | AES-256-GCM, AES-128-GCM, AES-256-CCM, AES-128-CCM |
| signing | yes | AES-GMAC, AES-CMAC; HMAC-SHA256 on 2.1 |
| compression | **no** | — |
| RDMA transform, transport capabilities (QUIC) | **no** | — |
| netname | read, not checked | — |

The server picks the first of its order the client offers. A 3.1.1 client that
offers no cipher the server shares is answered without encryption, and cannot
reach a share that requires it (§4.1).

> decision: compression is not offered. Content is already compressed where it
> pays, below the protocol ([RFC 5](rfc-5-transforms.md)), and per-message
> compression costs CPU on the `protocol` node for every read. Offer it when a
> WAN deployment shows reads limited by link bandwidth rather than by the node.

> decision: SMB Direct (RDMA) and SMB over QUIC are not offered. Both are
> transports the adapter does not have, and neither is needed by a client
> reaching the cluster over TCP. Offer SMB Direct with NFS over RDMA, when a
> deployment has RDMA fabrics; QUIC when clients must reach shares across the
> internet without a VPN.

### 2.3 Sizes and credits

- `MaxReadSize`, `MaxWriteSize` and `MaxTransactSize` are **8 MiB**, with
  multi-credit requests (one credit per 64 KiB).
- Each connection targets **512** credits outstanding. A response grants what
  the client asks, at least 1, up to the target; the server never grants past
  it and never revokes credits already granted.
- Sequence windows, credits and outstanding requests are per connection, in the
  memory of the `protocol` node.

> ponytail: one credit target for every connection. 512 keeps a client's
> pipeline full on a LAN and bounds what one client pins (RFC 24, planned).
> Make it adaptive to node memory when many idle connections, not few busy ones,
> are what exhausts a node.

## 3. Sessions

### 3.1 Session setup

`SESSION_SETUP` carries SPNEGO with Kerberos or NTLMv2 (NTLMv1 and LM refused);
what identity results, guest and anonymous access, and whether NTLM is allowed
at all are RFC 18's. The session key, signing key, encryption and decryption
keys and the preauthentication hash are held in the memory of the `protocol`
node, and **MUST NOT** be stored or sent to another node.

A share's signing and encryption flags (§4.1) are enforced per tree, not per
session: a session that is not encrypted may reach an unencrypted share and is
refused at tree connect to an encrypted one.

### 3.2 Multichannel only within one node

A session's channels **MUST** all be on one `protocol` node.

- `FSCTL_QUERY_NETWORK_INTERFACE_INFO` lists only the addresses the answering
  node holds now, so a client never discovers another node's.
- A `SESSION_SETUP` with `SMB2_SESSION_FLAG_BINDING` for a session this node
  does not hold is answered `STATUS_USER_SESSION_DELETED`
  (MS-SMB2 3.3.5.5.2); the client keeps the channels it has.

> decision: no multichannel across nodes. A session's keys, credits, open table
> and replay state live on the node that set it up ([RFC 17 §4.9](rfc-17-vfs.md)), and
> sharing them would make every request a cross-node lookup. Channels to one node
> still give the bandwidth multichannel is used for. Revisit if one node's NICs
> cap a single client's throughput below what a benchmark shows clients need.

### 3.3 Reconnect after a node is lost

When a `protocol` node is lost, its sessions, tree connects and volatile FileIds
go with it ([RFC 17 §4.9](rfc-17-vfs.md)). The client reaches a surviving node
through a floating address or Witness ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)), sets up a new session
(its `PreviousSessionId` names a session no node holds, and is ignored), tree
connects, and reconnects its durable and persistent opens (§5.4). Volatile opens
are lost, as after any server failure.

A lost `protocol` node loses no open state, so no grace runs; a lost primary is
a failover, and the reconnect is a reclaim in grace
([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)).

## 4. Trees and IPC$

### 4.1 Share flags

`TREE_CONNECT` is `Root` ([RFC 17 §4.9](rfc-17-vfs.md)). Its response maps
`ExportPolicy.SMB`:

| Policy | Wire |
| --- | --- |
| encrypt | `SMB2_SHAREFLAG_ENCRYPT_DATA`; an unencrypted request on the tree is `STATUS_ACCESS_DENIED` |
| require signing | an unsigned request on the tree is `STATUS_ACCESS_DENIED` |
| continuously available | `SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY` and `SMB2_SHARE_CAP_CLUSTER` (3.0+); persistent handles granted (§5.4); Witness served ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)) |
| hidden | nothing on the wire; omitted from enumeration (§4.2) |

Every share also reports `SMB2_SHAREFLAG_MANUAL_CACHING` (offline files only
when the user asks), `SMB2_SHAREFLAG_ENABLE_HASH_V2` off, and
`SMB2_SHARE_CAP_DFS`, `SCALEOUT` and `ASYMMETRIC` clear. `MaximalAccess` is
`Access` on the share root.

### 4.2 IPC$ and the RPC services

`IPC$` is the adapter's, reaches no file, and serves named pipes for exactly:

| Interface | Calls | Source |
| --- | --- | --- |
| srvsvc | `NetShareEnumAll`, `NetShareGetInfo` (levels 0, 1, 2, 501, 1005), `NetSrvGetInfo` | the share list, filtered as [RFC 17 §4.9](rfc-17-vfs.md) says |
| wkssvc | `NetWkstaGetInfo` (100, 101) | the installation's name |
| lsarpc | `OpenPolicy2`, `LookupSids2/3`, `LookupNames3/4`, `Close` | RFC 18's principals — the security tab cannot show names without it |

Every other call is answered `DCERPC_FAULT_OP_RNG_ERROR`. Witness is served over
TCP RPC, not on `IPC$` (§13).

> ponytail: three RPC interfaces, the subset Explorer, `net view` and the
> security tab call. Add an interface when a supported client workflow fails
> without it, never for completeness.

## 5. Opens and leases

### 5.1 CREATE

`CREATE` is one `Open` ([RFC 17 §4.1](rfc-17-vfs.md#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection)): disposition, desired access, share access as the
deny mode, a wanted lease and a durability, in one atomic step. Create options
and contexts map:

| Option or context | Maps to |
| --- | --- |
| `FILE_DELETE_ON_CLOSE` | pending delete ([RFC 14](rfc-14-open-state.md)) |
| `FILE_WRITE_THROUGH` | every write on the open is stable (§8.4) |
| `FILE_OPEN_REPARSE_POINT` | the symlink itself, not its target |
| `FILE_OPEN_BY_FILE_ID` | `STATUS_NOT_SUPPORTED`: no index from a numeric id to a file ([RFC 7](rfc-7-namespace-metadata.md)) |
| `MxAc` | `Access` |
| `QFid` | `FileId` as §8.1's `FileIdInformation` reports it |
| `RqLs` v1 and v2 | a lease request (§5.2) |
| `DHnQ`, `DH2Q`, `DHnC`, `DH2C` | durability and reconnect (§5.4) |
| `AlSi` | allocation size (§8.3) |
| `SecD` | an initial ACL through RFC 19 |
| `ExtA` | initial extended attributes (§8.5) |
| `TWrp` | the snapshot at that time (§14) |
| `AppInstanceId`, `AppInstanceVersion` | RFC 14's open identity; a second open with the same id closes the first |
| `SVHDX_OPEN_DEVICE_CONTEXT` | `STATUS_NOT_SUPPORTED` |

The FileId's persistent half **MUST** be derived from the RFC 14 open, so any
node finds the open from it; its volatile half names the entry in the
`protocol` node's table and is invalid on any other node.

### 5.2 Oplocks are leases

Every oplock is an RFC 14 caching grant, as a lease is: Level II is read,
exclusive is read and write, batch is read, write and handle. An oplock's
grant is keyed by its open, so two opens by one client break each other as
oplocks always have; a lease's by its lease key, so they do not. The lease key,
parent lease key and lease epoch are carried on the grant ([RFC 14](rfc-14-open-state.md)).

Directory leases are not offered: `SMB2_GLOBAL_CAP_DIRECTORY_LEASING` is
clear, and a lease request on a directory is granted none.

> decision: no directory leases, for NFS's reason ([RFC 21 §5.3](rfc-21-nfs.md#5.3%20Delegations)): every
> create in the directory breaks one. Revisit when a measured browse of a
> large share shows `QUERY_DIRECTORY` round trips dominating, with RFC 14's
> open question 4.

### 5.3 Breaks

A request that conflicts with a lease another client holds gets `ErrDelay`
from the service ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). SMB clients do not retry; the server
completes. So the adapter:

1. answers an interim `STATUS_PENDING` with an async ID, and frees the worker;
2. lets the primary send `LEASE_BREAK_NOTIFICATION` (or `OPLOCK_BREAK`) to the
   holder through its callbacks, with `ACK_REQUIRED` when write or handle
   caching is broken;
3. re-runs the request when the holder acknowledges, its break is revoked at
   the deadline ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)), or the client sends `CANCEL`, and
   completes the async response with the result.

A break from read to none needs no acknowledgement. The break deadline is RFC
14's; MS-SMB2's 35 s is the reference. A lease revoked at the deadline
invalidates the holder's opens under it, so its later writes through them are
`STATUS_FILE_CLOSED`.

> ponytail: the waiting node learns that a break ended by re-running the
> request on a backoff (10 ms doubling to 1 s), because the acknowledgement may
> arrive on another node. Notify the waiter from the primary when the
> retries show up in `CREATE` latency.

### 5.4 Durable and persistent handles

| Request | Granted when | Durability |
| --- | --- | --- |
| `DHnQ` (v1) | a batch oplock or a lease with handle caching is granted | durable, 60 s |
| `DH2Q` (v2), not persistent | as v1 | durable; the client's timeout, 60 s if zero, at most 300 s |
| `DH2Q` with `PERSISTENT` | the share is continuously available | persistent, same timeout bound |

Reconnect (`DHnC`, `DH2C`) is a `CREATE` that routes by its path to the file's
primary and finds the open by the FileId's persistent half. It succeeds only if
every one holds, and is otherwise `STATUS_OBJECT_NAME_NOT_FOUND`:

- the open is durable or persistent and its timeout has not run out;
- the session's principal is the open's;
- the tree's share is the open's ([RFC 17 §4.9](rfc-17-vfs.md));
- for v2, the `CreateGuid` and the connection's `ClientGuid` are the open's;
- the lease key, if any, is the open's.

After a primary failover the durable (not persistent) open was volatile and is
gone; the reconnect is then a **reclaim** during grace
([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)), accepted only from a client whose record names the shard, and
answered with no lease caching, since grants are not reclaimed
([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)). A persistent open is a stored record and reconnects with its
lease.

### 5.5 Replay

- A `CREATE` with `SMB2_FLAGS_REPLAY_OPERATION` and a `DH2Q` whose `CreateGuid`
  names an existing open returns that open (MS-SMB2 3.3.5.9.10), on any node,
  because the `CreateGuid` is with the open.
- Other replays (3.x `ChannelSequence`) are checked against the outstanding
  counts the `protocol` node keeps per open; all channels are on that node
  (§3.2).
- `LOCK` replay uses the open's `LockSequence` buckets ([RFC 14](rfc-14-open-state.md)): a request
  whose bucket holds its sequence number succeeds without re-applying. It is
  honoured on 3.x for durable and persistent opens; resilient handles
  (`FSCTL_LMR_REQUEST_RESILIENCY`) are not offered (§9), so 2.1 has none.

## 6. Byte-range locks

`LOCK` elements map onto RFC 14 locks under the open: shared or exclusive, a
64-bit range. SMB locks are mandatory against I/O from every protocol
([RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols)). A conflicting
lock with `FAIL_IMMEDIATELY` is `STATUS_LOCK_NOT_GRANTED`; without it the
request goes async, as §5.3 does, and completes on grant, `CANCEL`
(`STATUS_CANCELLED`) or close. A read or write across a range another open has
locked is `STATUS_FILE_LOCK_CONFLICT`. Unlock of a range not held is
`STATUS_RANGE_NOT_LOCKED`. A zero-length lock conflicts with nothing and is
held.

## 7. CHANGE_NOTIFY

`CHANGE_NOTIFY` registers an RFC 14 watch on the first request on an open and
reuses it for later ones; the completion filter is the watch's mask, and
`WATCH_TREE` its recursion. A request goes async and completes with the changes
`Notify` delivered ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). Changes arriving with no request outstanding are
buffered per open up to the request's `OutputBufferLength`; past it, the next
request completes `STATUS_NOTIFY_ENUM_DIR`, and the client re-lists. Closing the
open ends the watch; a request outstanding then completes
`STATUS_NOTIFY_CLEANUP`.

## 8. Information classes

### 8.1 Supported

| Query | Classes |
| --- | --- |
| file | `Basic`, `Standard`, `Internal` (RFC 7's numeric id), `Ea`, `Access`, `Position`, `Mode`, `Alignment`, `All`, `Stream`, `Compression` (always none), `NetworkOpen`, `AttributeTag`, `FullEa`, `Id` (volume serial and the 128-bit `FileID`), `NormalizedName` |
| directory | `Directory`, `FullDirectory`, `BothDirectory` and `IdBothDirectory` (short name empty), `IdFullDirectory`, `Names`, `IdExtdDirectory` |
| file system | `FsVolume` (serial and object id from `ShareID`, label the share name), `FsSize`, `FsFullSize` (the caller's quota where one applies), `FsDevice`, `FsAttribute`, `FsControl` (quota), `FsObjectId`, `FsSectorSize` (4096) |
| security | owner, group, DACL, SACL (§10) |
| quota | `SMB2_0_INFO_QUOTA`: per-user quotas, by SID through RFC 18 |

| Set | Classes |
| --- | --- |
| file | `Basic` (§8.2), `Rename` (§12), `Link`, `Disposition` and `DispositionEx` (pending delete, RFC 14), `EndOfFile`, `Allocation` (§8.3), `Position`, `Mode`, `FullEa` |

`FsAttribute` reports `CASE_PRESERVED_NAMES`, `UNICODE_ON_DISK`,
`PERSISTENT_ACLS`, `NAMED_STREAMS`, `SPARSE_FILES`, `SUPPORTS_HARD_LINKS`,
`SUPPORTS_REPARSE_POINTS`, `SUPPORTS_OBJECT_IDS`, `SUPPORTS_EXTENDED_ATTRIBUTES`
and `SUPPORTS_BLOCK_REFCOUNTING`; `CASE_SENSITIVE_SEARCH` only on a
case-sensitive share ([RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case)); maximum component length 255; and
the file system name `NTFS`.

> decision: the file system name is `NTFS`. Applications and installers refuse
> other names, and the flags above, not the name, say what is supported. Report
> another name only if a client is shown to infer from `NTFS` a feature this
> server lacks.

### 8.2 Not supported

| Class | Answer | Why |
| --- | --- | --- |
| `FileAlternateNameInformation` | `STATUS_OBJECT_NAME_NOT_FOUND`, what a volume with 8.3 names off returns | 8.3 names are not generated ([RFC 7](rfc-7-namespace-metadata.md)) |
| `FileShortNameInformation` (set) | `STATUS_NOT_SUPPORTED` | as above |
| `FileValidDataLengthInformation` (set) | `STATUS_NOT_SUPPORTED` | no valid-data length is kept; unwritten ranges read zeros |
| `FileFsLabelInformation` (set) | `STATUS_ACCESS_DENIED` | the label is the share name, set by the management API |
| `FileObjectIdInformation` (set) | `STATUS_NOT_SUPPORTED` | object ids are derived (§9) |
| mandatory label (`LABEL_SECURITY_INFORMATION`) | queried empty; set refused `STATUS_NOT_SUPPORTED` | no record holds it |

### 8.3 Timestamps, allocation and sparse files

**Timestamps.** In `FileBasicInformation`, −1 in a time suspends that time's
automatic update for the rest of the open's life; −2 resumes it (MS-FSA
2.1.5.14.2); an explicit time suspends it as −1 does. The flag **MUST** be held
with the open at the file's primary ([RFC 14](rfc-14-open-state.md)), not in the
adapter, because the primary sets `Modify` and `Change` at the existence commit
([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)), and a write through
a suspending open **MUST NOT** move them while a write through another open
still does. The flag is volatile: a reconnected durable open resumes updates.
Zero means "leave unchanged".

> decision: suspension is open state, not adapter state. In the adapter it
> could only rewrite times after the commit set them, which another node's
> `GETATTR` would see change twice. The cost is one field per open at the
> primary; nothing is stored.

**Allocation size** reported is `Charged` ([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)) rounded up to 4096.
Setting it below the end of file truncates (MS-FSA); above it is `Allocate` of
the range past the end, leaving the end of file where it is.

**Sparse files.** `FSCTL_SET_SPARSE` sets the sparse DOS attribute in `Flags`
and nothing else: holes exist on every file, sparse or not, and are RFC 6's.

### 8.4 Writes, flushes and stability

`WRITE` is an unstable `Write`; with `SMB2_WRITEFLAG_WRITE_THROUGH`, or on an
open with `FILE_WRITE_THROUGH`, a stable one. `FLUSH` is `Commit`. A short read
follows [RFC 17 §5.2](rfc-17-vfs.md#5.2%20Read).

### 8.5 Extended attributes

SMB extended attributes are RFC 7 xattrs — the records NFS's RFC 8276 xattrs
reach ([RFC 21 §9.3](rfc-21-nfs.md#9.3%20NFSv4.2%20operations)). SMB compares EA names without case; a set whose name
differs only in case from a stored name replaces that one.

## 9. FSCTLs

| FSCTL | Maps to | Notes |
| --- | --- | --- |
| `SET_ZERO_DATA` | `Deallocate` | sparse or not |
| `QUERY_ALLOCATED_RANGES` | `Seek` | data ranges, by `SEEK_DATA` / `SEEK_HOLE` |
| `SRV_REQUEST_RESUME_KEY` | the open | the key names the open in this node's table |
| `SRV_COPYCHUNK`, `SRV_COPYCHUNK_WRITE` | `Copy` | 256 chunks, 1 MiB each, 16 MiB per request; a key from another node is `STATUS_OBJECT_NAME_NOT_FOUND` |
| `DUPLICATE_EXTENTS_TO_FILE` (and `_EX`) | `Copy` as a clone | any alignment |
| `GET_REPARSE_POINT`, `SET_REPARSE_POINT`, `DELETE_REPARSE_POINT` | symlinks and junctions | other tags refused ([RFC 7](rfc-7-namespace-metadata.md)) |
| `CREATE_OR_GET_OBJECT_ID`, `GET_OBJECT_ID` | the 128-bit `FileID` | read-only; set and delete refused |
| `SRV_ENUMERATE_SNAPSHOTS` | snapshots | §14 |
| `QUERY_NETWORK_INTERFACE_INFO` | this node's addresses | §3.2 |
| `VALIDATE_NEGOTIATE_INFO` | the connection | §2.1 |
| `PIPE_TRANSCEIVE`, `PIPE_WAIT`, `PIPE_PEEK` | IPC$ | §4.2 |
| `OFFLOAD_READ`, `OFFLOAD_WRITE` (ODX) | `STATUS_NOT_SUPPORTED` | |
| `LMR_REQUEST_RESILIENCY`, `SET_COMPRESSION`, `FILE_LEVEL_TRIM`, `GET/SET_INTEGRITY_INFORMATION`, `SVHDX_*` | `STATUS_NOT_SUPPORTED` | |

> decision: no ODX. An offload token is a bearer capability to a range of a file
> that outlives the open that minted it, and would need its own record, expiry
> and revocation at the primary; copychunk and duplicate-extents give the same
> server-side copy without one. Offer it when a client workload shows copychunk
> limiting a copy that ODX would not.

## 10. Security descriptors

A security descriptor is RFC 7's ACL through RFC 19's mapping, and owner and
group SIDs are RFC 18's principals. The DACL is the ACL's entries; the **SACL
is the ACL's audit entries** ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)), stored with it, and reading or
writing it needs `ACCESS_SYSTEM_SECURITY`. Audit entries never grant or deny;
they select the access events the service emits ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)). A query for
parts the caller did not ask for returns none of them.

## 11. Names

### 11.1 Streams

A path component `name:stream` or `name:stream:$DATA` opens the named stream
(RFC 7's `Stream` file); `name::$DATA` and `name` open the file. Any other type
after the second colon is `STATUS_OBJECT_NAME_INVALID`. Stream names compare
without case on every share. `FileStreamInformation` lists `::$DATA` (files
only) and `:name:$DATA` for each stream.

### 11.2 Case

Case rules are the share's ([RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case)). SMB adds no folding of its
own. The SMB 3.1.1 POSIX extensions are not offered, so an SMB client cannot ask
for case-sensitive lookup on a case-insensitive share.

### 11.3 Names SMB cannot carry

A stored name is bytes ([RFC 7 §3.2](rfc-7-namespace-metadata.md#3.2%20A%20name%20is%20bytes%2C%20and%20it%20is%20validated%20at%20the%20boundary)); SMB carries UTF-16. Three cases:

1. **Valid UTF-8 with no character Windows reserves**: converted both ways.
2. **Valid UTF-8 containing a reserved character** — a control character, one of
   `" * : < > ? \ |`, or a trailing space or period — is shown with each such
   character replaced by its private-use code point (Appendix B); an SMB name
   containing those code points is converted back. A stored name that already
   contains one of those code points is treated as case 3.
3. **Not valid UTF-8**, or a name case 2 cannot round-trip: **hidden** from SMB.
   It is not listed, cannot be opened by name, and a directory holding only such
   names is not empty to `Remove` (`STATUS_DIRECTORY_NOT_EMPTY`).

A UTF-16 name with an unpaired surrogate is `STATUS_OBJECT_NAME_INVALID`.

> decision: hide rather than escape. Any escape of arbitrary bytes into UTF-16
> either loses bytes or collides with a name someone could type, and RFC 7
> forbids rewriting a name. A hidden name aliases nothing. The private-use
> mapping is the exception because it is reversible, and is the one SMB clients
> on other platforms already apply for the same characters. Revisit when a
> mixed-protocol share is shown to hold names in a legacy encoding; the answer
> then is a per-share charset, not an escape.

### 11.4 Reparse points

Symlinks are shown as `IO_REPARSE_TAG_SYMLINK`, and junctions as
`IO_REPARSE_TAG_MOUNT_POINT`; both are RFC 7 `Symlink`s. Opening one without
`FILE_OPEN_REPARSE_POINT` returns `STATUS_STOPPED_ON_SYMLINK` with the symlink
error response (MS-SMB2 2.2.2.2.1), and the client resolves it. Other tags are
refused ([RFC 7](rfc-7-namespace-metadata.md)).

## 12. Rename and delete with open handles

`FileRenameInformation` carries a path from the share root, resolved like a
`CREATE`'s. MS-FSA's rules apply across protocols, checked by RFC 14 at the
primary:

- the renaming open needs `DELETE` access, which every other open of the file
  must have shared;
- with `ReplaceIfExists`, a target that is open by anyone is
  `STATUS_ACCESS_DENIED`; without it, an existing target is
  `STATUS_OBJECT_NAME_COLLISION`;
- a directory with an open file or directory anywhere below it cannot be
  renamed: `STATUS_ACCESS_DENIED`.

A delete is a pending delete set by disposition or `FILE_DELETE_ON_CLOSE`, and
is RFC 14's; a later `CREATE` of the name is `STATUS_DELETE_PENDING`.

## 13. Witness

The Witness service (MS-SWN) is served by every `protocol` node over RPC on TCP,
through the endpoint mapper, for shares that are continuously available
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)):

- `WitnessrGetInterfaceList` lists every `protocol` node's floating addresses,
  each marked available or not;
- `WitnessrRegister` / `WitnessrRegisterEx` record the client, net name, share
  and address in the memory of the node registered with — which the client
  picks to be a different node from its SMB one;
- `WitnessrAsyncNotify` sends `RESOURCE_CHANGE` when an address moves,
  `CLIENT_MOVE` naming the target node when the client's node drains, and
  `IP_CHANGE` when a node's address list changes. `SHARE_MOVE` is not sent.

Registrations are not stored. A client whose Witness node is lost sees its
`AsyncNotify` fail and registers again elsewhere.

## 14. Previous Versions

`FSCTL_SRV_ENUMERATE_SNAPSHOTS` returns the `@GMT-YYYY.MM.DD-HH.MM.SS` token of
every complete snapshot of the share, newest first
([RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot)). A `CREATE` with a `TWrp` context, or a path whose
first component is a token, opens the file as of that snapshot. An open asking
for write access there is `STATUS_ACCESS_DENIED`; a token naming no snapshot is
`STATUS_OBJECT_NAME_NOT_FOUND`. The snapshot keeps the share's volume serial.

## 15. Profile containers

*Non-normative.* The reference workload ([index](rfc-index.md#Reference%20workloads))
stores each user's profile as one VHDX held open for the whole desktop session.
It exercises, and the conformance list covers:

- a `DH2Q` durable (or, on a continuously available share, persistent) open
  with a read-write-handle lease, held for hours (§5.2, §5.4);
- random 64 KiB in-place overwrites behind that one open, often write-through
  (§8.4);
- reconnect after a `protocol` node failover and after a primary failover, the
  second as a reclaim in grace (§3.3, §5.4);
- a sign-in storm: many users opening their containers at once, each reading a
  small metadata file and releasing it.

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

## 17. Conformance

| Requirement | Check | Fails without the rule |
| --- | --- | --- |
| §2.1 dialects | An SMB1-only and a 2.0.2-only negotiate are refused; a 3.1.1 client gets SHA-512 and its first shared cipher. | an SMB1 fallback left in |
| §3.2 multichannel | smbtorture `smb2.multichannel`; then bind a channel to a session on another node and assert `STATUS_USER_SESSION_DELETED`. | channels accepted across nodes |
| §3.3, §5.4 reconnect | smbtorture `smb2.durable-open`, `smb2.durable-v2-open`; a Windows client copying a large file while its `protocol` node is killed finishes the copy; repeat killing the primary. | sessions assumed durable; reconnect matched on FileId alone |
| §5.4 matching | Reconnect with another user, another share, another `CreateGuid`; assert each refused. | |
| §5.2, §5.3 leases | smbtorture `smb2.lease`, `smb2.oplock`; an NFS open against an SMB RWH lease breaks it and completes; a holder that never acks is revoked at the deadline and its next write fails. | breaks waited on by a worker |
| §5.5 replay | smbtorture `smb2.replay`; replay a `CREATE` with the same `CreateGuid` through another node. | |
| §6 locks | smbtorture `smb2.lock`; an SMB lock refuses an NFSv3 write across it. | locks held in the adapter |
| §7 notify | smbtorture `smb2.notify`; overflow returns `STATUS_NOTIFY_ENUM_DIR`; a change made over NFS is reported. | |
| §8 info classes | smbtorture `smb2.getinfo`, `smb2.setinfo`, `smb2.dir`, `smb2.streams`; MS-FSA test cases for the classes of §8.1. | |
| §8.3 timestamps | smbtorture `smb2.timestamps`; −1 on one open, write through it and through a second open on another node: only the second moves `LastWriteTime`. | suspension in adapter memory |
| §9 FSCTLs | smbtorture `smb2.ioctl` (copychunk, sparse, zero data, allocated ranges, duplicate extents). | |
| §10 SACL | Set a SACL with an audit entry, access the file; assert an access event and that `GETATTR` of `sacl` over NFS shows it. | |
| §11.3 names | Create over NFS a non-UTF-8 name and `a:b`; list over SMB. Assert the first absent, the second shown with U+F022 and openable; create `x?` over SMB and see `x?` over NFS. | an escape that collides |
| §12 rename | smbtorture `smb2.rename`, `smb2.sharemode`; rename a directory with a file below it open over NFS. | |
| §13 Witness | smbtorture `rpc.witness`; drain a node and assert a registered client receives `CLIENT_MOVE` and moves. | |
| §14 snapshots | smbtorture `smb2.twrp`; Explorer's Previous Versions tab lists and restores a file. | |
| §4.2 IPC$ | smbtorture `rpc.srvsvc`, `rpc.wkssvc`; a hidden share is absent from `NetShareEnumAll`. | |
| §15 profile containers | The reference workload's sign-in storm and an hour of random 64 KiB overwrites, with a `protocol` node and then a primary failed mid-run: every container mounts again without a repair prompt. | |

**What must not stand in.** A single-node run cannot fail §3.2, §3.3, §5.4 or
§5.5. A test client that never lets a break time out cannot fail §5.3.

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

1. **Lease after a reclaim.** Whether Windows clients accept a durable reconnect
   that returns no lease caching after a primary failover (§5.4), or treat it as
   a failed reconnect, is unmeasured.
2. **Open-below-directory check** (§12) needs RFC 14 to answer whether any open
   lies under a directory without walking the tree; how is RFC 14's.
3. **SMB 3.1.1 POSIX extensions** for Linux clients over SMB.
4. **Directory leases**, with NFS directory delegations and RFC 14's open
   question 4.

## Appendix A — prior art

*Non-normative.*

| Server | Choice | Taken or not |
| --- | --- | --- |
| Windows Server | SMB1 off by default; directory leases and ODX on; persistent handles on continuously available shares; Witness on clusters; break timeout 35 s | persistent and Witness taken; ODX and directory leases not |
| Samba | oplocks and leases over one lease table; private-use mapping for reserved characters; durable handles kept in a clustered database; multichannel per node | oplock-as-lease and the character mapping taken; durable records are RFC 14's |
| Clustered NAS with public-address takeover | sessions per node, channels per node, durable reclaim after takeover | taken (§3.2, §3.3) |

## Appendix B — private-use mapping

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
