---
rfc: 17
title: "RFC 17 — the filesystem service (VFS)"
component: vfs
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
aliases:
  - RFC 17
  - VFS
tags:
  - rfc
---
# RFC 17 — the filesystem service (VFS)

**Status:** draft.
**Audience:** anyone writing or changing a protocol adapter, and anyone changing
the order in which a client operation touches metadata, open state and content.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

The filesystem service sits between the protocol adapters and everything that
stores data. An adapter is a translator: it turns NFS or SMB messages into plain
calls such as open, write or rename. The service is the one place that runs each
call's checks, always in the same order: may this share be reached, may this
user do this, do others' opens or locks forbid it, is there quota. Picture a
hotel whose guests speak two languages: interpreters at the door translate, and
one front desk checks every request against the same rules. Unlike a front
desk, the service keeps nothing between calls. Each file's state lives with its
**primary**, the storage node in charge of the file. In a cluster the service
runs on protocol nodes and reaches storage nodes through the shard service. An
adapter never learns which node holds a file.

**What this is.** DittoFS speaks NFS and SMB. Each protocol has an **adapter**
that decodes requests off the wire and encodes the replies. Beneath them sits
one filesystem service, the single API every adapter calls. It puts the steps of
each client operation in order — admission to the share, the permission check,
the check against other clients' opens and locks, quota, then the data — and
runs the operation on the server that owns the file.

**The problem, with one example.** alice-pc keeps
`profiles/alice/ODFC_alice.vhdx` open over SMB all day, with a deny mode that
forbids anyone else to write while she holds it. A script on build01 writes to
the same file over NFSv3, a protocol with no notion of opening a file.

If each adapter put the steps together itself, the SMB adapter would check deny
modes, because SMB has them, and the NFSv3 adapter, written for a protocol
without opens, would not. The NFSv3 write would go straight into the journal, in
the middle of a virtual disk Windows has mounted. The disk is corrupt, and no
log anywhere shows a conflict.

With the service:

1. The SMB adapter decodes alice's 64 KiB WRITE, finds the open its SMB file ID
   names, and asks the service to write those bytes at that offset.
2. The service admits the call to the `profiles` share, sends it to the primary
   of the file's shard, checks the open's permission and the locks others hold,
   reserves quota if the file grows, and hands the bytes to the engine, which
   stages them in the journal. alice-pc gets OK.
3. build01's write arrives through the NFS adapter as a write on an
   **anonymous open**, which carries the caller's identity and no state. The
   same path checks it against the deny modes others hold, finds alice's, and
   refuses it as a sharing violation. The NFS adapter turns that into an NFS
   error.

One rule, in one place, for both protocols.

**Reaching a share works the same way.** Resolving a share's name or path,
admitting the call, the NFSv4 **pseudo-filesystem** and the list of shares a
client may see all belong to the service. An adapter only turns NFSv3 MOUNT,
NFSv4 PUTROOTFH, SMB TREE_CONNECT, SECINFO and share enumeration into the
service's share calls and lookups, and keeps no share list of its own.
Admission is not a door passed once at mount: it runs on every call, so an
NFSv4 client presenting a saved handle meets the same checks as one walking the
pseudo-filesystem into the share. Mounts, tree connects and sessions are never
stored.

```text
  alice-pc (SMB)                            build01 (NFSv3)
  WRITE 64 KiB, own open                    WRITE, same file, no open
        │                                         │
        ▼                                         ▼
  SMB adapter: decode,                      NFS adapter: decode,
  SMB file ID → alice's open                anonymous open
        │                                         │
        ▼                                         ▼
 ┌──────────────────────── filesystem service ─────────────────────────┐
 │ 1 admit to share `profiles`: state, flavour, client rules, squash   │
 │ 2 route to the primary of the file's shard                          │
 │ 3 authorise through the metadata store's one chokepoint             │
 │ 4 check open state: locks, deny modes, caching grants, grace        │
 │ 5 reserve quota if the file grows                                   │
 │ 6 engine: stage the bytes in the journal, return the verifier       │
 └─────────────────────────────────────────────────────────────────────┘
        │                                         │
   alice-pc: OK                     build01: refused at step 4,
                                    alice's deny mode
```

**The words you need.**

- **adapter** — the code for one wire protocol: framing, compounds, replay
  caches, error codes, and nothing else ([§4.2](#4.2%20Translation%20stays%20in%20adapters)).
- **primary** — the storage node that owns the file's shard and holds its
  namespace, open state and content; every call on the file runs there
  ([§4.8](#4.8%20One%20primary%20per%20file), [glossary](rfc-0-data-lifecycle.md#Glossary)).
- **open** / **anonymous open** — a client's open file, held as state / what an
  NFSv3 read or write runs against instead: the caller's identity, no state
  ([§4.1](#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection)).
- **admission** — the per-share checks every call passes before the permission
  check: the share's state, the authentication flavours it accepts, its client
  rules, and squashing, which maps the caller to the user it acts as
  ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)).
- **the chokepoint** — the one function in the metadata store that decides file
  permissions; the service is its only caller ([§4.6](#4.6%20One%20chokepoint)).
- **caching grant** — leave for a client to cache a file (an NFS delegation, an
  SMB lease or oplock). A conflicting operation starts a recall and tells its
  client to retry shortly ([§3.2](#3.2%20Callbacks)).
- **pseudo-filesystem** — the read-only tree of directories through which an
  NFSv4 client reaches each share's root, built the same on every node from the
  share list ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)).

**What this RFC promises.**

- Every call is admitted to its share, whatever path its handle arrived by, and
  the share grant is checked on every call: a client removed from a share is
  refused on its next call, cached handles or not.
- Conflicts across protocols — an NFS lock against an SMB deny mode, an NFS
  write against an SMB lease — are decided in one place, and the check and the
  I/O it admits run in one step at the primary, so nothing is granted between
  them.
- No worker waits on a client: a conflict that needs a recall answers "retry
  shortly", and a recall not acknowledged by its deadline revokes the grant.
- Adapters never see the cluster: a call sent to the wrong node is re-routed
  inside the service, and a write retried after a lost reply is applied once.
- Quota is enforced here and nowhere else, and writers spread over several
  primaries overshoot a hard limit by a stated bound at most.

**How the rest is organised.** §1 gives the purpose and non-goals; §2 only
explains the name. §3 is the interface — the operation groups, the callbacks to
adapters, the event hooks — and can be skimmed. §4 holds the rules, with §4.9 on
shares, mounts and the pseudo-filesystem. §5, the order of each operation, is
the core. §6–§8 are invariants, metrics and the conformance suite; §9 lists open
questions.

### Scenarios

**alice-pc's write reaches protocol node P1, and the file's primary is storage node S2.**

1. P1's SMB adapter decodes the WRITE and calls the service, exactly as it would
   on a single node ([§4.2](#4.2%20Translation%20stays%20in%20adapters)).
2. The service admits the call to `profiles` and finds that S2 is the primary of
   the file's shard ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees),
   [§4.8](#4.8%20One%20primary%20per%20file)).
3. It forwards the call through the shard service, the one interface between
   the two tiers, with a route envelope: request ID, shard and epoch, hop count
   ([RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut),
   [§4.7](#4.7%20Callable%20across%20the%20network)).
4. S2 checks opens and locks, reserves quota and stages the bytes, with no hop
   between the check and the write ([§4.8](#4.8%20One%20primary%20per%20file),
   [§5.1](#5.1%20Write)).
5. Before the write is acknowledged, it also reaches two replicas, say S1 and
   S3, at the default count ([RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)).

Outcome: four network crossings before OK, and the adapter never knew there
was a hop.

**The reply to build01's write is lost, and build01 sends it again.**

1. The resent NFSv3 WRITE carries the same request ID, built from the client's
   address, the XID, the procedure and a checksum of the arguments
   ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)).
2. Meanwhile another write has landed on the same bytes.
3. The primary finds the request ID in its dedup table and returns the first
   write's original result ([§4.7](#4.7%20Callable%20across%20the%20network)).
4. Had the primary changed in between, the new one would find that result in
   the write's own durable record ([§4.7](#4.7%20Callable%20across%20the%20network)).

Outcome: the retry is answered once, and never overwrites the newer write.

**build01 writes `profiles/alice/notes.txt` over NFS while alice-pc holds an SMB lease on it.**

1. The lease is a caching grant, leave for alice-pc to cache the file. The
   service sees the conflict and starts a recall, an SMB lease break
   ([§3.2](#3.2%20Callbacks)).
2. It answers build01 with `ErrDelay` at once, which the NFS adapter sends as
   the protocol's retry signal. No worker waits ([§3.2](#3.2%20Callbacks)).
3. build01 retries shortly; once alice-pc acknowledges the break, the write can
   go ahead.
4. If alice-pc never answers, the break is forced at its deadline, 35 s for
   SMB, and the lease drops to none ([§3.2](#3.2%20Callbacks)).

Outcome: a silent client costs others a retry interval, never a stuck server
thread.

**An administrator removes bob's grant on `profiles` while bob-pc still holds handles.**

1. bob-pc's next call presents a handle it got earlier.
2. The service admits the call, and the metadata store's one permission
   function evaluates bob's share grant again, as on every call
   ([§4.6](#4.6%20One%20chokepoint)).
3. No grant admits him now, so the call is refused. Holding a handle grants
   nothing ([§4.6](#4.6%20One%20chokepoint)).
4. Any cache of permission results has the grant in its key, so no stale "yes"
   survives ([§4.6](#4.6%20One%20chokepoint)).

Outcome: losing access takes effect on the next call, not at the next mount.

## In short

- Adapters speak wire. They call one protocol-neutral service, `vfs.Service`,
  and nothing else below them.
- The service is the only caller of the metadata store ([RFC 16](rfc-16-metadata-store.md)), open state
  ([RFC 14](rfc-14-open-state.md)) and the content engine ([RFC 8](rfc-8-engine.md)), and the only code that orders an
  operation across them.
- Its operation set is the union of what the protocols need, not their
  intersection. Translation, framing and replay stay in the adapters.
- It hides the topology: whether a primary is this process or another node
  ([RFC 15](rfc-15-topology.md)) never reaches an adapter.
- A file has one primary, which holds its namespace, open state and content. The
  conflict check and the I/O it gates run at that primary, in one step.
- It is the one enforcer of quotas, the one emitter of quota and access-audit
  events, and the one place operation latency is measured.
- Every call is admitted to its share — state, flavour, client rules, squash —
  whatever path its handle arrived by. Mounts, tree connects and sessions are
  never stored; the NFSv4 pseudo-filesystem is a pure function of the share list.
- It never blocks a worker on a client: a conflict that needs a recall returns
  `ErrDelay`, and the client retries.

---

## 1. Purpose

Every client operation touches several components: a write is authorised by the
namespace, checked against open state, charged to a quota, staged in the journal
and later recorded as existence. Something has to put those steps in order. This
document names that something and fixes where it stands:

```
   NFS adapter      SMB adapter      pNFS endpoints       wire only
        \                |                /
   ┌───────────────── vfs.Service ─────────────────┐
   │ open, close, read, write, commit, lookup,     │     the one API
   │ readdir, getattr, setattr, create, link,      │     adapters see
   │ remove, rename, lock, grants, watches, statfs,│
   │ mount, tree connect, pseudo-fs, share list    │
   └───┬────────────┬─────────────┬────────────┬───┘
   metadata      content        open state    routing
   (RFC 16)      (RFC 8)        (RFC 14)      (RFC 15)
```

[RFC 0 §1.3](rfc-0-data-lifecycle.md#1.3%20The%20layers) draws the same picture with the roles marked.

One service, rather than each adapter composing the parts itself, buys four
things:

1. **Orchestration is written once.** Written in each adapter, the orders drift,
   and a check one adapter has and the other lacks is how a client reaches data
   nobody audited.
2. **Cross-protocol rules have one home.** An NFS lock against an SMB deny mode,
   or an NFS write against an SMB lease, is decided where both are visible.
3. **The topology is invisible to adapters.** A call whose primary is another node
   is forwarded by the service; an adapter never learns there was a hop.
4. **It is testable without a protocol.** The service has its own conformance
   suite (§8), and the protocol suites then test translation only.

### 1.1 Non-goals

The service **MUST NOT**:

- encode or decode a wire format, run a compound or keep a replay cache —
  those are the adapters' ([§4.2](#4.2%20Translation%20stays%20in%20adapters));
- evaluate a file permission itself — it asks the one chokepoint ([§4.6](#4.6%20One%20chokepoint));
- hold a copy of metadata, open state or content that outlives the call that
  read it;
- persist anything of its own. Every durable fact is recorded by the component
  that owns it;
- import an adapter. Calls upward go through interfaces adapters implement
  ([§3.2](#3.2%20Callbacks));
- answer DFS referrals or NFSv4 referrals to another installation. A share is
  served only by the installation that holds it.

## 2. Name

`Filesystem` is taken three ways over: the metadata store's `FilesystemInfo`,
Go's `io/fs.FS`, and the product's own description. The word for this layer
across the field is **VFS** — the protocol-neutral file API between front-ends
and storage. The package is `vfs`, the interface `vfs.Service`, and an adapter
holds one value of it. Call sites read `svc.Open(…)`, never `vfs.VFS`.

## 3. Interface

Signatures are indicative; the obligations are normative. `Service` embeds the
groups, so an adapter holds one value and a test can fake one group.

`Identity` below is what the adapter authenticated — the protocol, the
authentication flavour and Kerberos level, the handshake's proof (RFC 18 §3.1)
or the numeric IDs an `AUTH_SYS` credential carried, and the client's address —
not yet mapped through any share's policy. The service resolves it per share
(§4.9) into the caller RFC 18 §6.1 defines — principal, groups, share access
narrowed by admission, and flags — and passes that to the metadata store as
its identity. Among the flags, `authenticated` is set when the flavour proved
who the caller is (Kerberos, an NTLMv2 SMB session), and `admin` when, on such
a flavour, the share grant names the principal `admin`
([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)); RFC 18 §6.5 decides which callers get each. A Kerberos principal the ID mapping maps to
uid 0 is privileged exactly when the export leaves root unsquashed, as an
AUTH_SYS root is. An `OpenRef` carries
the `Identity` of the call that presents it, so calls that name only an open
are admitted like every other.

### 3.1 Operations

```go
package vfs

type Service interface {
	Clients
	Names
	Attributes
	Data
	Locking
	Volumes
	Shares
	Identities
}

// Clients: who is talking to us, and how to call them back (RFC 14).
type Clients interface {
	Connect(ctx context.Context, c ClientInfo, cb Callbacks) (metadata.ClientID, error) // at SETCLIENTID_CONFIRM, the confirming CREATE_SESSION, SMB negotiate, an NLM host's first call; ErrClientInUse for an NFSv4 owner another principal holds (RFC 14)
	CreateSession(ctx context.Context, c metadata.ClientID, seq uint32, reply []byte) ([]byte, error) // NFSv4.1: a replay returns the cached reply, seq+1 records this one, any other ErrSeqMisordered (RFC 14)
	Rebind(ctx context.Context, c metadata.ClientID, cb Callbacks) error // the client's callback path is now through this node; the latest bind wins (§3.2)
	Renew(ctx context.Context, c metadata.ClientID) (ClientStatus, error) // the client's lease renewed; called by the answering node at most once per renewal interval and cached there; ClientStatus, below, is the lease owner's aggregate of what each primary reports (RFC 14 §5.3)
	Disconnect(ctx context.Context, c metadata.ClientID, session metadata.SessionRef) error // SMB: (protocol node, the node epoch the session was created under, session ID) (RFC 14 §2.2); the session's last channel ended; acts on that session's opens only (RFC 14 §8.1). (cluster) No one calls it for a protocol node that is lost: the primary treats every open whose session belongs to a protocol node whose node lease lapsed as disconnected at the lapse
	Destroy(ctx context.Context, c metadata.ClientID, by metadata.Principal) error // DESTROY_CLIENTID: ErrClientBusy while a session or state remains (RFC 14 §11)
	ReclaimComplete(ctx context.Context, c metadata.ClientID) error // RECLAIM_COMPLETE, recorded per (client, shard, grace instance) (RFC 14)
	FreeRevoked(ctx context.Context, c metadata.ClientID, s metadata.StateRef) error // NFSv4.1 FREE_STATEID of a revoked stateid; the NFSv4.0 and SMB adapters call it once they have answered the state revoked, since their clients have no free (RFC 14 §5.3)
}

// ClientStatus is what Renew reports about a client's state: the lease owner's
// combination of the revoked-set causes each primary reports (RFC 14 §5.3).
type ClientStatus struct {
	ReclaimNeeded bool // a shard of the client's state restarted its grace
	AdminRevoked      bool // a stateid an administrator revoked, of any kind — open, lock, delegation, layout — is in its revoked set, not yet freed (§3.2): SEQ4_STATUS_ADMIN_STATE_REVOKED
	RecallableRevoked bool // a delegation or layout revoked at its recall deadline is in its revoked set, not yet freed: SEQ4_STATUS_RECALLABLE_STATE_REVOKED
}

// Names: the namespace, by handle. Every method also takes a handle of the
// NFSv4 pseudo-filesystem (§4.9) and answers for it; an adapter never tells
// the two kinds apart.
type Names interface {
	Lookup(ctx context.Context, id Identity, dir Handle, name []byte) (Handle, metadata.File, error)
	ReadDir(ctx context.Context, id Identity, dir Handle, after Cursor, plus bool) iter.Seq2[DirEntry, error]
	Create(ctx context.Context, id Identity, dir Handle, name []byte, k CreateKind, a metadata.Attrs) (Handle, metadata.File, error) // mkdir, symlink, mknod, NFSv3 CREATE
	Link(ctx context.Context, id Identity, dir Handle, name []byte, target Handle) error
	Remove(ctx context.Context, id Identity, dir Handle, name []byte, dirOnly bool, via *OpenRef) error // via: an SMB delete through an open, a delete on close included, authorised by the open's grant (§4.6, §5.3); nil otherwise
	Rename(ctx context.Context, id Identity, from Handle, fromName []byte, to Handle, toName []byte, noReplace bool, via *OpenRef) error // via: an SMB rename through an open, authorised by the open's grant (§4.6); nil otherwise
}

// Attributes, ACLs, xattrs, streams.
type Attributes interface {
	GetAttr(ctx context.Context, id Identity, h Handle) (metadata.File, error)
	SetAttr(ctx context.Context, id Identity, h Handle, a metadata.Attrs, via *OpenRef) (metadata.File, error)
	GetACL(ctx context.Context, id Identity, h Handle) (metadata.ACL, error)
	SetACL(ctx context.Context, id Identity, h Handle, acl metadata.ACL, via *OpenRef) (metadata.File, error) // via: an SMB set-security through an open, authorised by the open's grant (§4.6); nil otherwise
	Xattrs(ctx context.Context, id Identity, h Handle) iter.Seq2[metadata.Xattr, error]
	SetXattr(ctx context.Context, id Identity, h Handle, x metadata.Xattr, mode XattrMode, via *OpenRef) error // via: an SMB EA set through an open, authorised by the open's grant (§4.6); nil otherwise
	RemoveXattr(ctx context.Context, id Identity, h Handle, name []byte, via *OpenRef) error   // via: as SetXattr
	Streams(ctx context.Context, id Identity, h Handle) iter.Seq2[metadata.File, error]
	Access(ctx context.Context, id Identity, h, via Handle, want Access) (Access, error) // NFS ACCESS, SMB MaximalAccess: Files.Granted ∩ want (RFC 7 §2.6); via is the directory h was resolved through, zero when unknown, so DELETE granted through the parent's delete-child is in the answer (§4.6)
}

// Data: opens, reads, writes. Every data call names an OpenRef: an open with
// the ClientID that holds it; an NFSv4 delegation with its ClientID, for I/O
// under a delegation stateid; or an anonymous one for NFSv3, which has no
// opens and carries the identity instead.
type Data interface {
	Open(ctx context.Context, id Identity, req OpenRequest) (OpenResult, error) // disposition, access, deny, grant wanted, durability; req names the ClientID, and the parent directory handle and name the open resolves through, passed to Authorize as via (§4.6)
	Close(ctx context.Context, c metadata.ClientID, o metadata.OpenID) error
	Read(ctx context.Context, o OpenRef, off int64, dst []byte) (n int, eof bool, err error) // n verified bytes may come with err (§5.2)
	Write(ctx context.Context, o OpenRef, off int64, src []byte, stable Stability) (n int, v WriteVerifier, err error) // ctx carries the call's HoldPolicy (§5.9)
	Commit(ctx context.Context, o OpenRef, off, length int64) (WriteVerifier, error) // NFS COMMIT, SMB FLUSH (§5.8); through an SMB open, ErrLost once after a loss (RFC 14 §8.1)
	Deallocate(ctx context.Context, o OpenRef, off, length int64) error // punch a hole; ordered as a write (§5.5)
	Seek(ctx context.Context, o OpenRef, off int64, what SeekWhat) (int64, error) // SEEK_DATA, SEEK_HOLE
	Copy(ctx context.Context, src OpenRef, srcOff int64, dst OpenRef, dstOff, length int64) (int64, error) // server-side copy and clone (§5.9); ErrInvalid for overlapping extents of one file or a clone over clone_max_len; ErrCrossNamespace across namespaces
	PreWarm(ctx context.Context, id Identity, dir Handle, recursive bool) (Progress, error) // the service enumerates the files it may read, the engine fetches them
}

// Locking: byte-range locks, caching grants, watches (RFC 14).
type Locking interface {
	// Lock is all or nothing: if any range cannot be granted, the ranges this
	// call took are released before it answers that range's error. wait is
	// honoured only for a single range: a conflict leaves a waiter (RFC 14),
	// answered ErrLocked (NFSv4 polls) or ErrBlocked (NLM, SMB).
	Lock(ctx context.Context, o OpenRef, rs []metadata.LockRange, wait, reclaim bool) error
	CancelLock(ctx context.Context, o OpenRef, r metadata.ByteRange) (granted bool, err error)     // drops a waiting lock, or reports it granted first; called before any answer to an ErrBlocked request but the grant (RFC 14 §2.10)
	TestLock(ctx context.Context, o OpenRef, r metadata.ByteRange, exclusive bool) (*metadata.Lock, error)
	// Unlock releases rs in order and stops at the first range the owner does
	// not hold, ErrRangeNotLocked; the ranges before it stay unlocked. An NFSv4
	// or NLM owner holds every sub-range it locked, so it never meets the error.
	Unlock(ctx context.Context, o OpenRef, rs []metadata.ByteRange) error
	ReturnGrant(ctx context.Context, c metadata.ClientID, g metadata.GrantID) error
	AckBreak(ctx context.Context, c metadata.ClientID, g metadata.GrantID, to GrantKind) error
	Watch(ctx context.Context, c metadata.ClientID, id Identity, dir Handle, recursive bool, filter metadata.ChangeMask) (metadata.WatchID, error)
	Unwatch(ctx context.Context, c metadata.ClientID, w metadata.WatchID) error
	LayoutGet(ctx context.Context, o OpenRef, r metadata.ByteRange, write, reclaim bool) (metadata.Layout, error) // pNFS
	LayoutCommit(ctx context.Context, c metadata.ClientID, l metadata.LayoutID, end int64, mtime time.Time) error // ErrBadLayout on a stale epoch
	LayoutReturn(ctx context.Context, c metadata.ClientID, l metadata.LayoutID) error
}

// Shares: how a client reaches a share, and which shares it may see (§4.9).
// An adapter passes the name or path the client sent and never holds a ShareID.
type Shares interface {
	Mount(ctx context.Context, id Identity, path string) (Handle, []AuthFlavor, error)     // NFSv3 MNT
	PseudoRoot(ctx context.Context, id Identity) (Handle, error)                             // NFSv4 PUTROOTFH, PUTPUBFH
	TreeConnect(ctx context.Context, id Identity, name string) (Handle, ShareInfo, error)    // SMB TREE_CONNECT
	SecInfo(ctx context.Context, id Identity, dir Handle, name []byte) ([]AuthFlavor, error) // SECINFO, SECINFO_NO_NAME (empty name)
	ListShares(ctx context.Context, id Identity) iter.Seq2[ShareInfo, error]                 // NetShareEnum, showmount -e
}

// Volumes: what statfs, FSSTAT and SMB volume queries ask.
type Volumes interface {
	StatFS(ctx context.Context, id Identity, h Handle) (metadata.FilesystemInfo, error) // the share's, or the tighter of it and h's project (RFC 7 §2.8)
	Quota(ctx context.Context, id Identity, h Handle, p metadata.Principal) (metadata.Usage, metadata.Quota, error) // p's own quota, else the share's default for its kind
}

// Identities: how an adapter encodes and decodes a principal — a file's owner,
// an ACL entry's who, a quota's target, a name in an lsarpc lookup. It holds
// no policy: squashing and grants are applied at admission (§4.9), not here.
// The mapping is RFC 18 §4's; an adapter never derives one spelling from
// another, and an error other than not-found reaches the client as a server
// failure, never as an unknown principal.
type Identities interface {
	Spell(ctx context.Context, p metadata.Principal, scheme Scheme) (Spelling, error) // p's spelling in scheme: uid, gid, SID, Kerberos name, owner string; ErrNoSpelling when it has none
	Find(ctx context.Context, s Spelling) (metadata.Principal, error)                 // the principal holding s; ErrUnknownPrincipal when none does
}
```

**A protocol request naming several ranges is one call.** An SMB `LOCK` of
several elements is one `Lock` or one `Unlock` carrying every range in order;
NFSv4 and NLM pass a one-element list. Rollback of a partial acquisition is the
service's, never the adapter's: the ranges go to the file's one primary in one
call, which takes them all or none ([RFC 14 §2.3](rfc-14-open-state.md#2.3%20Lock)).

Every open-state call names the `ClientID` it acts for, and the service
**MUST** refuse, with `ErrNotYours`, an `OpenID`, `GrantID`, `WatchID`,
`LayoutID` or lock owner that the named client does not hold. A leaked or forged ID then reaches
nothing, and a client whose lease expires releases its watches with the rest of
its state ([RFC 14 §4.3](rfc-14-open-state.md#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)).

**Naming the right client is not enough.** Where a protocol names the client
only through the state it presents, as an NFSv4.0 stateid does, anyone who
learns that state could present it. An `OpenRef`'s open-time grant
([§4.6](#4.6%20One%20chokepoint)) therefore authorises a call only when the call's principal is the
principal the open was granted to, or, for an NFSv4.1 client under
machine-credential state protection, the client's machine principal for the
operations it protects. A call by any other principal through the same open,
lock state or layout **MUST** be refused with `ErrNotYours`
([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), whatever that principal could
do to the file on its own: the state is the opener's, and a principal that wants
its own access opens the file itself.

**A delegation is the client's, not an opener's.** An NFSv4 client may do I/O
under its delegation's stateid for any of its open-owners and principals
(RFC 8881 §9.1.3), so an `OpenRef` naming a delegation is not bound to a
principal. The service **MUST** refuse it with `ErrNotYours` unless the
delegation is held by the call's client — for NFSv4.1 the session's — and
**MUST** then authorise the call as for an anonymous open: the caller's own
principal against the file and the share grant ([§4.6](#4.6%20One%20chokepoint)),
and against the deny modes held. The delegation lends no access.

> decision: there is no space reservation, so `Allocate` is not offered. NFS
> `ALLOCATE` is answered not supported, and an SMB allocation size is accepted
> and changes nothing but what the adapter reports. A reservation would promise
> that a later write cannot fail for space, which a journal shared by many shares
> and drained by offload cannot keep. Add one, as a per-share charge against the
> journal's capacity, if a workload is shown to need `ALLOCATE`'s guarantee.

### 3.2 Callbacks

```go
// Callbacks is implemented by each adapter and registered per client at
// Connect. The service calls up through it and never imports an adapter.
type Callbacks interface {
	RecallGrant(ctx context.Context, g metadata.CachingGrant, to GrantKind) error // NFS CB_RECALL, SMB lease or oplock break
	Notify(ctx context.Context, w metadata.WatchID, changes []Change) error      // SMB CHANGE_NOTIFY, NFS directory notifications
	Revoked(ctx context.Context, what Revocation)                                // lease expiry, administrative revoke, share removed or moved (RFC 14 DropShare), one grant revoked, a shard's state lost: the adapter answers any request it holds pending on the revoked state
	LockWaitOver(ctx context.Context, w metadata.LockWaiter, granted bool) (WaitAnswer, error) // NLM_GRANTED, SMB lock completion (granted); NFSv4.1 CB_NOTIFY_LOCK, optional (reserved, not granted)
}

// Revocation names state the service ended without the client asking. Share is
// set for RevokedShareRemoved and RevokedMoved, where only that share's state
// ends; Grant for RevokedGrant, where only that caching grant ends (RFC 14 §5.3);
// Shard for RevokedShardState, where only the state that shard held ends
// (RFC 14 §4.4). RevokedLeaseExpired and RevokedByAdmin end every piece of state
// the client holds (RFC 14 §4.3, §5.3).
// Until the client frees what was revoked (FreeRevoked), Renew reports it by
// cause — the revoked set records each entry's — in ClientStatus.AdminRevoked
// or RecallableRevoked;
// a call naming revoked state — a grant revoked at its recall deadline, or any
// state an administrator revoked — is refused ErrRevoked, never ErrNotYours,
// and one naming lost shard state ErrStaleState (§4.3). An administrator's
// revoke keeps the client record and its sessions, so the client frees what was
// revoked and opens afresh under its own client ID (RFC 14 §5.3).
// A removed or moved share's state is the exception: DropShare deletes the
// records that could list it, so a call naming it is refused ErrBadState and no
// flag is set, since nothing could ever clear one (RFC 16 §2.3.2, RFC 14).
type Revocation struct {
	Client metadata.ClientID
	Share  metadata.ShareID
	Grant  metadata.GrantID
	Shard  metadata.ShardID
	Reason RevokeReason
	Addr   netip.Addr // RevokedMoved: the address the move names, which the adapter stops serving; zero when the move names none (RFC 27 §2.2)
}

type RevokeReason uint8

const (
	RevokedLeaseExpired RevokeReason = iota // the client's lease expired
	RevokedByAdmin                          // an administrator revoked the client; its record and sessions are kept (RFC 14 §5.3)
	RevokedShareRemoved                     // RFC 14 DropShare with the removed reason, for the share's removal
	RevokedGrant                            // one caching grant whose recall missed its deadline
	RevokedShardState                       // a shard's open state was lost
	RevokedMoved                            // RFC 14 DropShare with the moved reason, which the old owner's release runs for each moved share (RFC 27 §2.2); Share is set
)

// WaitAnswer is what the client said to a LockWaitOver.
type WaitAnswer uint8

const (
	WaitAcked   WaitAnswer = iota // NLM: the host acknowledged NLM_GRANTED; SMB and NFSv4: the completion or notification was sent
	WaitRefused                   // NLM: the host refused the grant; the primary releases the lock
)
```

`LockWaitOver` returns an error when the client gave no answer by the call's
deadline. For an NLM grant the service then calls it again, and keeps the lock:
a grant is never released for a silence, only for a refusal, a cancel, the
host's restart, an administrator's revoke of its client, or its share's removal ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)).

A callback **MUST NOT** block the service beyond its context's deadline, and a
callback that fails or times out **MUST** be treated as the client not answering:
a recall that is not acknowledged by its deadline revokes the grant
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).

**No worker waits on a recall.** An operation that conflicts with a caching
grant starts the recall and returns `ErrDelay` at once. The adapter maps
`ErrDelay` to the protocol's retry signal — NFS4ERR_DELAY or NFS3ERR_JUKEBOX —
and the client retries. Where the protocol has none, as for SMB, whose client
does not retry a request answered with an interim (pending) response, the
adapter sends the interim response and re-drives the call itself, within the
call's deadline, answering the client once the call completes or the deadline
passes. The adapter chooses each hold below by the cause the `ErrDelay` carries (§4.3),
never by guessing from the operation. A request the adapter holds pending this way **MAY** carry a deadline
longer than the default of [§4.3](#4.3%20Errors%20are%20neutral%20values), up to
`smb_pending_deadline` ([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)), which **MUST** exceed the SMB break
deadline of [RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)
(35 s; called the **break deadline** below), so that the request outlives the
recall it waits on and completes when the break resolves rather than failing
just before it does. One answered `ErrGrace` **MUST**
be held until the shard's grace ends plus the break deadline, so that a new open made during
grace is granted when grace ends rather than failed while grace still refuses it
([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)).
One answered `ErrDelay` because its share is quiesced or frozen for a move
**MUST** be held the same way, until the freeze ends plus the break deadline and never longer
than `migration.freeze_timeout` plus the break deadline
([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)), rather than failed at `smb_pending_deadline` while
the freeze still holds; if the share's namespace has moved when the freeze ends,
the old owner's release runs RFC 14's `DropShare` with the moved reason for each
moved share, which ends every open, lock, waiting lock, grant and watch under it
and reports them `RevokedMoved`; the adapter answers what it holds pending on
them — a waiting lock, an outstanding change notification, a held request.
`DropShare` with the moved reason carries the address the move names, or
none, in `Revocation.Addr`. **From the release the adapter stops serving that
address entirely**: it drops every NFS call arriving on it, with no reply, so
a hard mount retries, closes every SMB connection on it, and accepts no new
connection there ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)). A moved share's tree reached through any
other address is ended `STATUS_NETWORK_NAME_DELETED` — or, for a continuously
available share, every connection carrying a tree of it is closed, so the
client's reconnect runs — and the connection keeps every other share's trees
otherwise. The client reaches the new owner through the address
the move hands it ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)), never through this installation, which
answers no referral (§1.1).
Only persistent opens, whose records the move carries, reconnect there; durable
and resilient handles are held only at the old primary and are refused, so the
client reopens, as after a failover ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)). One answered `ErrDelay` for a transient capacity refusal is held
up to `smb_pending_cap` ([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)), which **MUST** exceed
`migration.freeze_timeout` plus the break deadline, and only there answered `STATUS_DISK_FULL` (§4.3).
One that reaches its deadline still `ErrDelay` because the metadata store has
stalled — its own transaction, or the existence commit a stable write waits on,
outstanding or failing — or waiting on the version limit's raise or on quota
slack, **MUST** be held and re-driven the same way up to `smb_pending_cap`, and
only there answered `STATUS_UNEXPECTED_IO_ERROR`, as a failed read is (§5.2):
each waits on metadata commits, and none is a lack of space. One still
`ErrDelay` with `DelayConflict` is held the same way up to `smb_pending_cap` and
only there answered `STATUS_FILE_NOT_AVAILABLE`, the retry-later status a
freeze or grace ends with: a conflict is contention, not a failure. One
answered `ErrDelay` with `DelayHoldBudget` or `DelayResources` is never held: it is answered
`STATUS_INSUFFICIENT_RESOURCES` at once (§4.3, §5.9). One answered `ErrUnavailable` — a read, query or
IOCTL that needs content the remote tier cannot serve now — **MUST** be held and
re-driven the same way up to `smb_pending_cap`, and only there answered
`STATUS_UNEXPECTED_IO_ERROR`: the cause clears by itself, unlike `ErrLost`. SMB has no retry-later status, so answering at the call's
deadline would fail, for a stall NFS clients ride out by retrying, every write a
Windows client had in flight.
A lock request answered `ErrBlocked` has no deadline at all: it holds no worker,
and it ends only as its waiter does
([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)); an adapter that
answers it other than as granted calls `CancelLock` first and answers only once it
returns. Either way no service worker waits: the retry is the client's or the
adapter's, and holds no state at the primary. A client that holds grants on many files and never
acknowledges therefore costs each conflicting operation one retry interval,
never a pinned worker. After one of a client's recalls is revoked, the service
**MUST** stop offering that client grants and recall the others it holds
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
An SMB break that times out is forced — the lease downgraded to none — and the
request held behind it proceeds; the break keeps the opens it
covered, with their locks and deny modes, so the holder may keep writing through
them; a revoked NFS delegation's later writes are refused (the same section).

**A callback path can move between nodes** (cluster, [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). A client's
callbacks go out through the protocol node that holds its back channel. When the
client binds its callback path through another node — after an address takeover,
or a lost connection — that node calls `Rebind`, and the service records it in the
client's record ([RFC 14](rfc-14-open-state.md)); the latest bind wins, and a recall in flight is
retried through it. Without the record, every recall after the client moved
would go out through a path the client no longer listens on and end in
revocation.

**Notifications honour traverse permission.** `Notify` delivers a change only
when the watch's identity may traverse every directory between the watched one
and the change, evaluated through the chokepoint ([§4.6](#4.6%20One%20chokepoint)) at delivery. A recursive
watch never reports names below a directory its holder cannot enter.

### 3.3 Event hooks

```go
// Events is implemented by an event sink supplied at composition. The
// service calls it and never waits on it.
type Events interface {
	Quota(ctx context.Context, e QuotaEvent)   // advisory threshold crossed, soft limit exceeded, grace expired, hard refusal, back under
	Access(ctx context.Context, e AccessEvent) // one authorisation decision the audit policy selects
}

type AccessEvent struct {
	Time       time.Time
	Share      metadata.ShareID
	Client     metadata.ClientID // zero for NFSv3
	Addr       netip.Addr        // the client's address
	Identity   Identity
	Protocol   Protocol
	Op         Op
	File       metadata.FileID
	Name       []byte // the name acted on, for namespace operations
	Want       Access
	Allowed    bool
	SelectedBy Selector // Policy: the share's audit policy; Entry: an audit or alarm entry of the file's ACL, which the chokepoint reports with its decision (RFC 7 §7.6, RFC 19 §3.7)
}

// QuotaEvent is one quota transition or refusal. Transitions are emitted once
// each; hard refusals are coalesced by the sink per share, scope, subject and
// resource (RFC 25 §4.4), which stamps the count it coalesced.
type QuotaEvent struct {
	Time      time.Time
	Share     metadata.ShareID
	Scope     QuotaScope         // User, Group, Project or Share
	Subject   metadata.Principal // the user or group; zero for a project or share quota
	Project   metadata.ProjectID // the tree; zero otherwise
	Resource  QuotaResource      // Bytes or Files
	Kind      QuotaEventKind     // AdvisoryCrossed, SoftExceeded, GraceExpired, HardRefused, BackUnder
	Used      int64              // the usage judged, in the resource's unit
	Soft      int64
	Hard      int64
	GraceEnd  time.Time          // zero when no grace runs
	Writer    Identity           // the refused caller, for HardRefused only
}
```

Both streams are emitted from the one path every operation takes, by this
service and nothing else: access events where the service consults the
chokepoint ([§4.6](#4.6%20One%20chokepoint)), quota events where it enforces the quota ([§5.6](#5.6%20Quota)). The chokepoint
returns its decision, and with it whether an audit or alarm entry of the file's
ACL selected it ([RFC 7 §7.6](rfc-7-namespace-metadata.md#7.6%20Decisions%20are%20observable)); it never reads the share's audit policy.
The service applies that policy to the decision and the entry flag, decides
whether an event is due, and builds and emits it. No adapter,
store or engine emits either, so no protocol can reach a file without appearing
in the audit stream.

- An event hook **MUST NOT** block or fail the operation. The sink queues, and
  an event it cannot queue is dropped and counted by the sink (RFC 25's
  `events_dropped_total`).
- Which operations produce an access event — every decision, denials only,
  writes and namespace changes, per share — and whether the file's audit
  entries select events too (RFC 19 §3.7) is the audit policy, a setting
  ([RFC 13](rfc-13-configuration.md)). With no policy, no access event is built, whatever audit entries
  a file holds.
- Delivery, retention and the export format of both streams belong to RFC 25.
  This RFC fixes only where they are emitted and what an event carries.

## 4. Rules

### 4.1 The operation set is the union, not the intersection

`Open` takes a create disposition, requested access, a deny mode, a wanted
caching grant and a durability, because SMB needs create-open-deny as one
atomic step and NFSv4's `OPEN` has the same shape. NFSv3 has no opens: its reads
and writes run against an **anonymous open**, which carries the identity and
no state. An operation one protocol needs and the other lacks — `Copy`, `Seek`,
`Watch`, streams — is in the set; an adapter whose protocol lacks it simply does
not call it.

A protocol's semantics **MUST NOT** be approximated by composing weaker service
calls in the adapter. If SMB's atomic create-with-deny were built from a lookup,
a create and an open, two clients could interleave between them. The service
offers the atomic form, or the operation is not supported.

### 4.2 Translation stays in adapters

Adapters own, and the service never sees:

- wire encoding and framing (XDR, SMB2), NFSv4 `COMPOUND` and SMB compounding,
  which run as sequences of service calls;
- stateid and FileId encodings, sequence and replay caches;
- turning `MNT`, `PUTROOTFH`, `TREE_CONNECT`, `SECINFO` and share enumeration
  into the `Shares` calls of §4.9, and nothing more: no share resolution,
  admission or pseudo-filesystem logic lives in an adapter;
- security descriptors ↔ `metadata.ACL`, applying RFC 19's mapping;
- mapping the service's errors to protocol status codes;
- a protocol's own numeric file IDs, read from the file's stored number
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)).

### 4.3 Errors are neutral values

The service returns `ErrNotFound`, `ErrExist`, `ErrAccess`, `ErrPerm`, `ErrStale`,
`ErrNotEmpty`, `ErrNotDir`, `ErrIsDir`, `ErrNoSpace`, `ErrQuota`, `ErrLocked`, `ErrRangeNotLocked`,
`ErrShareViolation`, `ErrGrace`, `ErrDelay`, `ErrBlocked`, `ErrNotYours`, `ErrRevoked`, `ErrBadLayout`, `ErrWrongSecurity`, `ErrInvalid`, `ErrCrossNamespace`, `ErrCrossShard`, `ErrCrossProject`, `ErrInconsistent`, `ErrClientInUse`, `ErrClientBusy`, `ErrSeqMisordered`, `ErrStaleClient`, `ErrStaleState`, `ErrBadState`, RFC 14's `ErrNoReclaim`, `ErrReclaimRevoked`, `ErrDeletePending`, `ErrNoCopy` and `ErrBadSeqID`, `ErrIncomplete`, and the content errors of
[RFC 8](rfc-8-engine.md) (`ErrLost`, `ErrUnavailable`, `ErrCorrupt`). Each adapter maps them
once. `ErrDelay` means "retry shortly", and every one the service returns is a
`*Delay` that `errors.Is(err, ErrDelay)` matches and that names its cause, with
the bound the cause sets where it has one. Both are declared in the errors
package the engine and the service share, so the engine returns them without
importing the layer above:

```go
// HoldPolicy rides in the call's context (§5.9). The service counts every
// call that carries one from the call's first hold or ErrDelay until its
// context ends, re-runs under that context included, whatever held it. An
// absent policy, or a budget of 0, sets no cap of that kind: the call's
// deadline still bounds every hold. The service alone counts holds; an
// adapter keeps no second count.
type HoldPolicy struct {
	OnHold     func() // called once, when the service begins holding the call
	Key        HoldKey // what the service counts holds under, across calls
	SlotBudget int     // how many of the key's calls the service may count at once; 0: no cap
	ByteBudget int64   // how many bytes of the key's writes the service may count at once; 0: no cap
	Pooled     bool    // the budgets are a floor: past one, a call draws on the node's hold pool while it has room (§5.9)
}

// HoldKey names a session or connection uniquely across the installation.
type HoldKey struct {
	Node   metadata.NodeID // the front-end protocol node
	Epoch  uint64          // that node's node epoch, so a restarted front-end's numbering never meets the dead process's holds (as RFC 14's SessionRef)
	Holder uint64          // an NFSv4.1 session, or an SMB connection, of that node and epoch
}

type Delay struct {
	Cause DelayCause
	Until time.Time // the freeze's or grace's end, where the cause has one; zero otherwise
}

type DelayCause uint8

const (
	DelayRecall     DelayCause = iota // a caching grant is being recalled (§3.2)
	DelayClone                        // a running clone holds the extent past the call's deadline (§5.9)
	DelayFrozen                       // the share is quiesced or frozen for a move; Until is the freeze's bound
	DelayMoved                        // the share's namespace has moved away (§3.2)
	DelayCapacity                     // the journal's capacity or a share's journal limit (RFC 8 §10.2)
	DelayVersionLimit                 // the version limit's raise is under way (RFC 8 §10.2)
	DelayStoreStall                   // the metadata store commits nothing (RFC 8 §11.2)
	DelayQuotaSlack                   // the reservation slack is spent (§5.6)
	DelayConflict                     // a metadata conflict still unresolved at the call's deadline (RFC 0 §9.2)
	DelayHoldBudget                   // the call's hold budget is spent, and the node's hold pool too where it may draw on it (§5.9)
	DelayResources                    // a node memory pool is spent: the share cap, open state, or a reply reservation's deadline (RFC 24)
)
```

The causes and their answers are [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s table for space and
[RFC 8 §11.2](rfc-8-engine.md#11.2%20Every%20condition%20in%20RFC%200%20%C2%A710%20has%20its%20engine%20behaviour%20here) for a stalled store; this section cites them rather than restating
them, and a conflict the service still meets at the call's deadline — its
retries under the deadline spent — is `ErrDelay` with `DelayConflict`, never an
I/O error ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)). Adapters map it to `NFS4ERR_DELAY`
or `NFS3ERR_JUKEBOX`, and NLM to `NLM4_DENIED_GRACE_PERIOD`; SMB holds the
request pending and re-drives it (§3.2). `DelayHoldBudget` is answered at
once and never held: NFSv4.1, the one NFS transport that passes a policy,
maps it to `NFS4ERR_DELAY`, and SMB to `STATUS_INSUFFICIENT_RESOURCES`
(RFC 22). `DelayResources` is answered at once and never held, like
`DelayHoldBudget`, since holding it would spend the hold pool on the share that
is short of memory: `NFS4ERR_DELAY` over NFSv4, `NFS3ERR_JUKEBOX` over NFSv3,
`NLM4_DENIED_NOLOCKS` over NLM and `STATUS_INSUFFICIENT_RESOURCES` over SMB
(RFC 24 §4.2). **A transient refusal never becomes
`ErrNoSpace` because a deadline ran out**: a space refusal still `ErrDelay` when
the caller's deadline passes is answered `ErrDelay` — the NFS client retries, for
as long as the outage lasts — and an SMB request held pending for it is answered
`STATUS_DISK_FULL` only once it has been held for `smb_pending_cap`
([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings); longer than `migration.freeze_timeout` plus the break deadline, §3.2).
`ErrNoSpace` is answered at once, and only for a permanent cause: the second
row of [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s table, which is the one list of them. `ErrBlocked` means a lock
request waits and will be granted through `LockWaitOver` (§3.2); NLM maps it to
`NLM_BLOCKED`, SMB to a pending response. `ErrNoSpace` means no space will come
without operator action; `ErrQuota` means a logical quota
refused. Both are answered at once, and no other space refusal is. Every
`ErrQuota` is a `*QuotaError` that `errors.Is(err, ErrQuota)` matches, whose
`Scope` names the quota that refused — `QuotaShare`, `QuotaProject`,
`QuotaUser` or `QuotaGroup`, the first exceeded in that order. A share-wide or
project quota is the volume a client sees, its room what `StatFS` reports, so
adapters answer it as no space; a user or group quota is answered as a quota
exceeded (RFC 21, RFC 22). `ErrInvalid` means the arguments
cannot be honoured as given — an overlapping copy or clone within one file, a
clone longer than `clone_max_len` (§5.9), or a deny ACL entry naming a
well-known principal with no row in
[RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)'s table, which every walk would skip — and maps to `NFS4ERR_INVAL` and
`STATUS_INVALID_PARAMETER`, as Linux answers `EINVAL`. `ErrCrossNamespace` means
a copy or clone whose source and destination are in different namespaces, which
share no chunks to adopt (§5.9); it maps to `NFS4ERR_XDEV` and
`STATUS_NOT_SUPPORTED`, so the client falls back to reading and writing.
`ErrCrossShard` means **(cluster)** a by-reference copy or clone — `COPY`,
`CLONE`, copychunk or duplicate-extents — whose source and destination are in
different shards, whatever their primaries
([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)), refused at admission, changing nothing (§5.9). For a `COPY`
or copychunk the adapter answers no error: it copies the bytes itself through
the service, reading at the source and writing at the destination in bounded
chunks, and answers the count, as RFC 21 and RFC 22 state. For a `CLONE` or
duplicate-extents request it maps to `NFS4ERR_XDEV`, never `NFS4ERR_NOTSUPP`,
which a client records as the operation missing for the whole mount, and to
`STATUS_NOT_SAME_DEVICE`, so the client copies the bytes itself. `ErrCrossProject` means a rename or link across tree quotas
([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)), and maps to `NFS4ERR_XDEV`, `NFS3ERR_XDEV` and
`STATUS_NOT_SAME_DEVICE`, as Linux answers `EXDEV`, so the client copies and
deletes. `ErrInconsistent` means a store invariant is broken — a count that
would underflow, a constraint the server's own state breaches
([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface), [RFC 6 §10](rfc-6-block-metadata.md#10.%20API%20surface%20and%20observability)) — and maps to `NFS4ERR_IO`, `NFS3ERR_IO`
and `STATUS_UNEXPECTED_IO_ERROR`, never `ErrExist`; the store raises a health
condition naming the record. `ErrClientInUse` and `ErrSeqMisordered` are RFC 14's
client-record refusals, mapped to `NFS4ERR_CLID_INUSE` and
`NFS4ERR_SEQ_MISORDERED`.
`ErrStaleClient` means the call names a client that expired, or
is marked by the loss of a shard's open state until it repeats `SETCLIENTID`
([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)), and
`ErrStaleState` a stateid or FileId whose state may have been lost with its
shard's open state: one whose record is missing while its shard is in grace, or
whose grace-instance bits name an earlier grace instance than the shard's
current one — the instance advances only when the shard's open state is lost,
at a restart or a failover that loses it, never at a handover that ships it
([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client), [§4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)), so the client reclaims; they map to `NFS4ERR_STALE_CLIENTID` for an
operation naming the client ID — for one naming a stateid, as RFC 21 maps it —
and to `NFS4ERR_STALE_STATEID`. `ErrBadState` means every other stateid or FileId
whose record is missing — expired, revoked and freed, a removed or moved
share's, or never issued — and maps to
`NFS4ERR_BAD_STATEID` and `STATUS_FILE_CLOSED`; over SMB, where a lost open is a FileId the server no
longer holds, `ErrStaleState` maps to `STATUS_FILE_CLOSED`.
`ErrPerm` means the operation is refused whatever the caller's access — an
immutable or append-only file ([RFC 7 §7.8](rfc-7-namespace-metadata.md#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint)), or a change of owner, group or
flags only a privileged caller or the owner may make ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)) — and maps
to `NFS4ERR_PERM`, `NFS3ERR_PERM` and `STATUS_ACCESS_DENIED`, as Linux answers
`EPERM`.
`ErrGrace` means the file's shard is in grace and the request needs, or
conflicts with, state that may still be reclaimed (§5.1). `ErrWrongSecurity`
means the share's policy does not admit the call's flavour (§4.9); NFSv4 maps it
to `NFS4ERR_WRONGSEC`, NFSv3 to the RPC authentication error `AUTH_TOOWEAK`
(RFC 5531's `auth_stat`; RFC 2623 §2.4 allows an NFS error or an RPC
authentication error), as RFC 21 does — a Linux client returns `EACCES` either
way, as for the `NFS3ERR_ACCES` some servers answer, and the RPC error adds a
client log line naming the cause — and SMB to an access error.
`ErrRevoked` means the call names state in its client's revoked set, not yet
freed — a grant revoked at its recall deadline, or any state an administrator
revoked, its client record and sessions kept (§3.2,
[RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) — and maps, as RFC 8881 answers it, to
`NFS4ERR_ADMIN_REVOKED` for any stateid an administrator revoked, a
delegation's and a layout's included, and `NFS4ERR_DELEG_REVOKED` only for a
grant revoked at its recall deadline (RFC 8881 §15.1), with
`SEQ4_STATUS_ADMIN_STATE_REVOKED` or `SEQ4_STATUS_RECALLABLE_STATE_REVOKED` set
until `FREE_STATEID` frees each.
`ErrClientBusy` means `Destroy` found the client still holding state, and maps to
`NFS4ERR_CLIENTID_BUSY`. `ErrReclaimRevoked` means a reclaim in a shard where an
administrator revoked the client's state, refused until the client next takes
state there
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)), and maps to `NFS4ERR_NO_GRACE`. It is NFSv4-only: an
administrator's revoke of an NLM host is an expiry that deletes its record, so
the host is not notified, an NLM reclaim after it is `ErrNoReclaim`, and a
later lock of it starts a new client. RFC 14's four refusals map as RFC 21 and
RFC 22 state them. `ErrIncomplete` means a read of a file a snapshot
lists as incomplete ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), in the snapshot or in any clone,
restore or backup made from it, which carries the list, or a copy or clone
whose source is such a file, refused before anything changes (§5.9); it maps to
`NFS4ERR_IO`, `NFS3ERR_IO` and `STATUS_UNEXPECTED_IO_ERROR`, never to the file's
older content. A routing refusal — wrong primary, stale epoch — **MUST NOT** reach an
adapter: the service re-routes and retries within the caller's deadline ([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20primaries)).

An operation that arrives with no deadline is given one of 30 s from its
arrival at the service — up to `smb_pending_deadline` for a request an adapter holds pending, until
grace's end plus the break deadline for one refused `ErrGrace`, until a freeze's end plus the break deadline
for one refused because its share is quiesced or frozen, up to `smb_pending_cap`
for an SMB request held on capacity, a store stall, the version limit, quota
slack, a conflict or `ErrUnavailable` (the causes RFC 13's `smb_pending_cap` row names), and none for a lock
request answered `ErrBlocked` (§3.2) — fixed rather than a setting ([RFC 13](rfc-13-configuration.md)), so every wait below
it ends ([RFC 0 §10.3](rfc-0-data-lifecycle.md#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)).

> decision: the default deadline is fixed at 30 s, not measured. It is a
> backstop for calls that name no deadline of their own, and every internal wait
> is sized below it, so a setting would let an operator break bounds other RFCs
> derive from it (the single-node self-fence of
> [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) is
> one). Revisit it if a measured operation class completes correctly but past
> 30 s under load, or if a client is shown to give up on a call sooner.

### 4.4 Handles are opaque

The service mints handles through the metadata store
([RFC 7 §6](rfc-7-namespace-metadata.md#6.%20Handles)). An adapter stores and returns them, and **MUST NOT** build,
parse or compare their contents.

### 4.5 No copies on the data path

`Read` fills the caller's buffer and `Write` takes the caller's. The service
passes them to the engine, which lends or streams below
([RFC 8](rfc-8-engine.md)); the service **MUST NOT** add a copy of its own. Buffer budgets
are RFC 24 §3's.

### 4.6 One chokepoint

Authorisation stays in the metadata store's `Files.Authorize`
([RFC 7 §7.1](rfc-7-namespace-metadata.md#7.1%20One%20chokepoint)). The service calls it on every operation that needs it, and is
its only caller. An open-time grant ([RFC 7 §7.2](rfc-7-namespace-metadata.md#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always)) is computed there and stored
with the open; the service evaluates it on the operations it gates.

- **The share grant is checked on every call**, not only at mount or tree
  connect. The service reads the caller's grants at admission each time
  (§4.9 step 4), `Authorize` applies the access they give as the share ceiling,
  and any cache of authorisation results **MUST** include the share's grant-list
  version, which a grant's deletion moves as well as its change, and the `via`
  directory below with its whole record's version — owner and group as well as
  mode and ACL, since the group bits and the sticky rule read them — in its
  key ([RFC 7 §7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read)). A
  client removed from a share loses access on its next call, cached handles or
  not.
- `Mount`, `TreeConnect` and a `Lookup` into a share take the caller's identity and are refused when no grant admits it.
- **An open is decided with its parent.** The service passes `Authorize` and
  `Granted` the directory an open, a remove, a rename or an `Access` resolved
  through as `via` ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)), so an SMB open asking for `DELETE`, or
  for `MAXIMUM_ALLOWED`, is granted it when the parent grants deleting a child,
  as removing the name over NFS is.
  Holding a handle grants nothing: a handle names a file, and every call on it
  is authorised afresh.
- **A call through an open uses the open's grant.** A write, a set-information,
  and an SMB rename, delete, delete on close, set-security or EA change through
  an open (`via` on `SetAttr`, `Rename`, `Remove`, `SetACL`, `SetXattr` and
  `RemoveXattr`) pass the metadata store an `OpenGrant` built from
  that open's record, its `Access` and `Privileged` fields ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)) — its file, the access granted at open, whether it is privileged
  (an SMB open with backup or restore intent by a caller privileged for it)
  and the directory it resolved through ([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)) — and are evaluated against it, not
  re-checked against the directory or the file afresh, so a rename through a
  handle opened with `DELETE` needs no new delete-child decision. A handle
  opened with backup or restore intent by a caller privileged for it holds a
  privileged grant, and every request through it is privileged — owner, group
  and DACL set included — as [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved) states; `MaximalAccess` is still
  computed without the privilege.
- **Deny modes.** A granted open's deny mode is evaluated once, at open
  ([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)). An NFSv3 anonymous open has no open to evaluate
  at, so each of its reads and writes is checked against the deny modes held
  by others: a read under deny-read and a write under deny-write are refused
  with `ErrShareViolation` — after the handle-caching grants covering the
  conflicting opens are broken, as for an open (§5.3).

### 4.7 Callable across the network

A call may reach a node that does not own the file ([RFC 15](rfc-15-topology.md)). Every
operation **MUST** therefore take and return values and resume an iteration
from a cursor the caller holds.

A call the service forwards (cluster, [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)) carries RFC 15's **route envelope**: a request ID,
unique across the cluster, the shard and epoch the sender expects, and a hop
count. The primary refuses a call whose epoch is not its own, and keeps a short
dedup table keyed by request ID alone that returns a mutation's original result
to a retry. The table is not shipped at a handover
([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)): a journal operation or namespace mutation keeps its
result in its own durable record, which the new primary reads on a miss, and
only the open-state calls' entries, which leave no durable result, ship with
the open-state table ([RFC 14 §10](rfc-14-open-state.md#10.%20Shard%20placement), [RFC 8 §12.2](rfc-8-engine.md#12.2%20A%20retried%20call%20is%20recognised%2C%20not%20re-applied)).
So a write whose reply was lost, retried after another write landed on the same
extent, returns the first write's result instead of overwriting the second.
Repeating a mutation with the same arguments is **not** assumed to be safe.

### 4.8 One primary per file

Every call on a file runs at the primary of the file's shard
([RFC 11](rfc-11-ownership.md)), which holds the file's namespace records, its open state and
its content. A node with the `protocol` role only forwards
([RFC 15](rfc-15-topology.md)). The service is composed on the `protocol` role, over a local
view of the shards its node is primary of and a remote view of the rest; the
remote view is the shard service, the one interface that crosses to a storage
node ([RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)). Adapters hold the service and never either view.

The conflict check and the I/O it gates therefore run at one primary, with no
hop between them. The primary **MUST** order them so that no lock, deny mode or
grant is granted between the check and the I/O it admits: a conflicting grant
is ordered wholly before the check, which then refuses, or wholly after the
I/O. The service does no cross-primary orchestration for a single-file
operation. The operations that touch two primaries are those
[RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries) and [RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards) name — a rename, link or unlink whose
files are in several shards, and a create whose new file lands in a shard other
than its directory's, which draws the new file's version at the new file's
primary (§5.1) — which it orders through one coordinator, and `ReadDir` with
attributes, which reads each entry's attributes at its own primary
([§5.7](#5.7%20GetAttr)) and needs no order between them. A `Copy` between files of two
shards, whatever their primaries, is not among them: nothing is adopted by
reference across shards, so a `COPY` runs as reads at one primary and writes at
the other, and a `CLONE` is refused with `ErrCrossShard` (§4.3, §5.9,
[RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)).

### 4.9 Shares, mounts and trees

**Admission runs on every call.** Before it authorises, the service admits the
call to the share of the handle or open it names, in this order:

1. the share's state ([RFC 16 §2.3.1](rfc-16-metadata-store.md#2.3.1%20Share%20names%2C%20paths%20and%20state)) — `ErrDelay` while quiesced,
   `ErrStale` once being removed or retired ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share));
2. the share's `ExportPolicy` flavour rule — `ErrWrongSecurity`;
3. its client rules, against the call's address and the netgroups it is in —
   `ErrAccess`, or a narrowing to read-only — and the policy's `ReadOnly` flag,
   which narrows every caller to read-only;
4. resolution, which turns the `Identity` into the caller it acts as on this
   share: map it to a principal, squash it, collect its groups, read its share
   access from the grants, and set its flags (RFC 18 §6). Step 3's narrowing to
   read-only, by a client rule or `ReadOnly`, reaches the chokepoint as a field of that identity, since the
   chokepoint evaluates no client rule (RFC 19 §3.2);
5. the share's cap on the node's request buffers (RFC 24 §4.2) — `ErrDelay`
   with `DelayResources`, at once — except a call that only releases state
   (close, unlock, lock cancel, grant or layout return, freeing a state ID) or
   reclaims it, with the operations that carry it, which steps 1–4 and
   `Authorize` still gate (RFC 24 §4.2).

Then `Authorize` applies the share access as a ceiling and evaluates the file
(§4.6). **There is no call
that skips admission**: lock, unlock, test-lock, layout, watch, grant return and
the calls that name only an open are admitted like a read. Admission is a
property of the share a call reaches, not of how it reached it, so an NFSv4
compound that `PUTFH`s a handle and one that walks the pseudo-filesystem into the
share meet the same five checks. A compound crossing from one share into another
is re-admitted, and re-squashed, at the crossing. The result of steps 1–4 may be
cached per (share, identity) only with `ExportPolicy.Version`, the netgroups'
versions, `ShareList.Version`, the share's grant-list version
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)'s `S‖id‖gv`) and the principals
generation in the key, beside everything RFC 18 §7.1 lists; step 5 reads the
pools as they are and is never cached.

**A share's removal joins the calls admitted before it.** The service counts,
per share at each primary, the calls it has admitted and not yet answered.
Once admission refuses the share as being removed, the removal's step 3
([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)) waits for that count to reach zero before it ends the share's
open state, alongside the engine's own join
([RFC 8 §2.3](rfc-8-engine.md#2.3%20One%20engine%20per%20node%3B%20a%20share%20is%20a%20context)): a create, link or rename enters the engine only to draw its versions, which counts nothing, so only
this count holds one admitted before the mark from committing a record after
the removal has passed it.

**The service owns every step of reaching a share; the adapter only
translates.** What the three protocols share — resolving a name or path through
RFC 16's indexes, admission, the pseudo-filesystem, what `SECINFO` answers and
which shares a client may list — is written once, here, behind the `Shares`
group and the pseudo handles `Names` accepts. NFSv3 `MNT` is `Mount`; NFSv4
`PUTROOTFH` is `PseudoRoot` followed by `Lookup`s; SMB `TREE_CONNECT` is
`TreeConnect`. Each resolves, admits and authorises, and returns the share's
root handle. They are where a client fails *early*; they are not where it is
gated, since every later call is admitted again. An adapter holds no share list,
and a change to the share list reaches every protocol at once.

> decision: share resolution and the pseudo-filesystem live in the service, not
> the adapters, because the rules for reaching a share drifted when each
> adapter had its own copy: a check present on one entry path was missing on
> the other. Move a piece back to an adapter only if a protocol needs a rule the
> others must not share, and then state it in that protocol's RFC.

**Neither is stored.** No record of a mount, a tree connect or an SMB session
exists in the metadata store:

- the NFSv3 MOUNT table is not kept. `DUMP` returns an empty list and `UMNT`
  and `UMNTALL` succeed and change nothing, which every client tolerates; a
  table kept per node would be wrong after any failover anyway;
- SMB sessions, tree connects and their IDs live in the protocol node's memory.
  A client that reconnects — after a restart, or an address takeover
  ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)) — sets up its session and tree connects again, then
  reclaims durable or persistent opens ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)). A reclaim is refused
  unless the reclaiming tree connect's `ShareID` is the open's;
- `ClientID` is the only client state that is stored, and it is RFC 14's.

**The NFSv4 pseudo-filesystem is a function of the share list.** The service
on every `protocol` node builds it from the share path index
([RFC 16](rfc-16-metadata-store.md)), and must build the same one, because a client moved between
nodes by an address takeover or `fs_locations` presents the handles it already
holds:

- a pseudo directory's handle is minted by the handle codec as its own kind
  ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)), from a digest of the installation's identity and the
  directory's path, and nothing else; so is its numeric file id. Every node and every restart mints
  the same. A pseudo handle whose path no longer leads to a share is `ErrStale`;
- its `fsid` is one reserved value per installation, never a share's; its
  change attribute is `ShareList.Version`, so adding, removing, renaming or
  re-pathing a share is seen by every client's cache;
- a share's root reports the share's own `fsid`, and `mounted_on_fileid` the
  numeric id of the pseudo directory it sits in, so a client sees a mount
  crossing there;
- a pseudo directory lists only the components that lead to a share the caller
  would be admitted to. `SECINFO` on a pseudo directory answers every flavour some share
  admits; on a share's path, the share's `Flavors`.

**Share enumeration** — SMB `NetShareEnum` over `IPC$`, NFS `showmount -e` —
is `ListShares`: the shares whose policy is not hidden and to which the caller
would be admitted. `IPC$` is the SMB adapter's RPC endpoint, not a share, and
reaches no file.

> decision: the MOUNT table is not kept, so `showmount -a` lists nothing. It is
> advisory in the protocol and no client depends on it; keep one only if an
> operator workflow is shown to need it, and then per node and labelled as such.

## 5. Orchestration

The service is the only code that orders a client operation across components.
Each order below is normative.

### 5.1 Write

1. resolve the handle; refuse a stale one; admit the call to its share (§4.9);
   route to the primary (§4.7, §4.8);
2. through an open, refuse with `ErrNotYours` a caller that is not the
   principal it was granted to (§3.1), and evaluate the open's stored grant;
   through a delegation, refuse with `ErrNotYours` a call from a client that does
   not hold it (§3.1); for a delegation or an anonymous open, authorise the
   caller against the file ([§4.6](#4.6%20One%20chokepoint)) and check deny modes
   held by others, breaking handle caching before refusing (§5.3);
3. if the shard is in grace, refuse with `ErrGrace` a write that overlaps a lock,
   deny mode or grant that may still be reclaimed ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)); otherwise
   check open state: the caller's open and byte-range locks held by others
   ([RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols)). A conflicting caching grant starts a recall and the write
   returns `ErrDelay` (§3.2);
4. reserve the charge the write adds, if it extends the file (§5.6);
5. refuse with `ErrPerm` a write to an immutable file, and one to an
   append-only file below its size, releasing step 4's reservation, before
   anything else changes
   ([RFC 7 §7.8](rfc-7-namespace-metadata.md#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint)); then, if the caller is not privileged — the owner included — and the file is
   setuid, or setgid with group execute, clear those bits in a committed mode
   change, which the append-only flag does not refuse: it is the write's own
   consequence, not a change the caller asked for
   ([RFC 7 §9.6](rfc-7-namespace-metadata.md#9.6%20setuid%20and%20setgid%20are%20cleared%20when%20an%20unprivileged%20caller%20changes%20the%20file)).
   The same step runs for a truncate, a deallocate, and the destination of a
   copy or clone (§5.5, §5.9);
6. call the engine's `Write`, which stages the bytes in the journal and returns
   the write verifier ([RFC 8](rfc-8-engine.md)), passing the times
   the caller's open has suspended ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)). A suspended `Modify` or
   `Access` is not advanced for this write, by the overlay, the existence commit or
   a replay; `Change` and `Version` advance regardless ([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)).
   The service holds the file's accept lock
   ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)) across the engine's `Write` alone, and inside that hold
   checks step 5's flags again against the size the overlay reports, so a flag
   set or a size changed since is honoured; a truncate, deallocate, copy or
   clone into such a file is refused in the same hold (§5.5, §5.9). For a stable
   write it then releases the lock and calls the engine's `Commit` over the
   written extent ([RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)): the lock never spans a sync or a group
   commit. Every other change of a file — an attribute, flag, ACL, xattr, link
   or directory change, step 5's clearing — takes its version from the engine's
   `Draw` under the same lock and passes it to the metadata store's call, which
   stores it with the file's floor entry ([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)). Each file draws from its
   own journal: a create draws its parent's version from the parent's and the
   new file's from the new file's, at that file's primary when it lands in
   another shard. The lock spans the draw, not the store's transaction, so a
   write may be accepted at a higher `Version` in between, committed or only
   staged; the change is then refused `ErrVersionBehind` by
   [RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)'s check, which its commit transaction runs once against the
   stored `Version` and the highest version any staged write of the file drew, and the service draws again under the lock
   and retries, within the call's deadline, never surfacing it; one still
   refused at the deadline answers `ErrDelay` with `DelayConflict` (§4.3)
   ([RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more));
7. reply. A stable write is answered once the journal has synced it **and**, if
   it overwrites committed content, once its existence commit has landed — the
   overwrite record that keeps a later loss reading as **Lost** rather than as
   the content it replaced. A stable append or hole fill waits only for the sync.
   Existence, usage and `mtime` are committed by the engine's group commit, not
   here, and a replay after a crash restores what was pending
   ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)); that commit releases the reservation.

**Which writes are stable.** A write is stable when the client asks for it (NFS
`FILE_SYNC` or `DATA_SYNC`, SMB write-through), and every write through a
persistent open, or through any open on a continuously available share, is
stable whatever the client asked: those opens survive a restart, and an
acknowledged write they carry must survive it too. A durable open that is not
persistent is not reclaimable across a restart of the primary
([RFC 14](rfc-14-open-state.md)), so its writes need not be.

A write the journal cannot take — the share's journal limit or the journal's
capacity — returns `ErrDelay`, not `ErrNoSpace`, while offload or repack can
free space, and stays `ErrDelay` when the caller's deadline passes; only an SMB
request held to `smb_pending_cap` is answered no space (§4.3). `ErrNoSpace` is
answered at once for a permanent cause alone
([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)), and a logical quota refuses at once (§5.6).

Steps 3–6 run at the primary as one step (§4.8), except across a hold (below).

**One lock order, for every call.** A call takes what it needs in this order,
and never the reverse: first the open-state serialisation of every file it
touches — the step in which open state checks and admits it (§4.8,
[RFC 14 §11](rfc-14-open-state.md#11.%20Interface)) — in file-ID order; then fold admission at every
directory whose deltas it writes, and at a directory whose own attributes it
changes, that a rename moves, or whose `Subdirs` it recounts, which run behind
fold admission as a fold does; fold admission at several directories is taken
in file-ID order within the call's deadline, and a call that cannot take them
all by then answers `ErrDelay` with `DelayConflict`, so no cycle forms
([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps), [§9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)); then the accept locks of
every file it draws for, in file-ID order. A call queued for fold admission
holds no accept lock, and one waiting for an accept lock holds every
open-state step it needs already. So a write, which holds its file's
open-state step when it takes the accept lock, and an unlink of that file's
last name, which needs the same open-state step in one step with its commit,
never each hold what the other waits for. [RFC 7](rfc-7-namespace-metadata.md), [RFC 8](rfc-8-engine.md) and
[RFC 14](rfc-14-open-state.md) cite this order rather than restate it.

**A held call holds nothing.** A call the service holds — behind a clone or a
copy's chunk (§5.9), or for any other cause — releases its accept lock, every
open-state step it took and step 4's reservation for the length of the hold,
and when the hold ends — the clone or chunk done — re-runs steps 3–6 from the
start, re-checking grace, open state and
step 5's flags as if it had just arrived. Holding them, a write held in a
cloning source extent would keep the source's open-state step while the
clone's last batch waits on that step to delete its clone-kind open; the two
would wait on each other until the write's deadline, and every open, close,
lock and break acknowledgement on the file with them, long enough for a lease
break to run out and be revoked.

### 5.2 Read

Steps 1–3 as for a write, with read access; then the engine's `Read`, which
resolves residency and streams verified chunks
([RFC 8 §7.2](rfc-8-engine.md#7.2%20The%20reply%20streams%2C%20one%20verified%20chunk%20at%20a%20time)). It never returns an unverified byte, but it can
return `n` verified bytes and then an error (`ErrLost`, `ErrUnavailable`,
`ErrCorrupt`) for the chunk after them. Each adapter answers that once:

- **NFSv3 and NFSv4** answer a short read of the `n` bytes, with `eof` false,
  when `n > 0`; the client re-reads from `off + n` and then gets the error
  (`NFS3ERR_IO` / `NFS4ERR_IO`, or `NFS4ERR_DELAY` / `NFS3ERR_JUKEBOX` for
  `ErrUnavailable`). With `n = 0` the error is the reply.
- **SMB2/3** answers a short read likewise when `n > 0` and the request's
  `MinimumCount` is met; otherwise the error, `STATUS_UNEXPECTED_IO_ERROR`, or
  `STATUS_FILE_CORRUPT_ERROR` for `ErrCorrupt`. `ErrUnavailable` is transient,
  so with `n = 0` the request is held pending and re-driven up to
  `smb_pending_cap` (§3.2) before it is answered with the error.

A reply is never padded with zeros for the bytes that failed.

`SEEK_DATA`, `SEEK_HOLE` and the allocated ranges of `READ_PLUS` and SMB's
allocated-range query come from the engine's `Allocation`
([RFC 8 §7.6](rfc-8-engine.md#7.6%20Allocation%20answers%20from%20the%20hole%20set)), which counts a zero ref under a newer overwrite record as data;
no adapter derives them from reads.

### 5.3 Open and close

`Open` resolves or creates the name, authorises, checks deny modes against the
opens already held, offers a caching grant if no other client conflicts, and
records the open — all at the file's primary, in one step as the protocol
sees it. `Close` drops the open after checking that the named client holds it.

**A sharing violation is decided after handle caching is broken.** A request
whose deny-mode check fails against opens that a handle-caching grant covers —
an SMB lease with handle caching keeps handles open that its client has already
closed — **MUST NOT** be refused yet, whatever its protocol: an SMB or NFSv4
open, an anonymous NFSv3 read or write, I/O under a delegation, an NFS `REMOVE`
or `RENAME` against a deny-delete
([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)).
It starts a break of those grants and returns `ErrDelay` (§3.2), and on its
retry re-checks the deny modes against the opens that remain. Refused at once,
it fails against handles nobody is using.

**The service runs every release.** It calls the engine's `Release`
([RFC 8 §8.1](rfc-8-engine.md#8.1%20A%20removal%20is%20one%20transaction%2C%20then%20batches)) at the file's primary, at four moments: when `Unlink` or `Rename`
reports a file left with no entry and no open holds it; when open state's `Close`
reports the last close of a file with no entry, having deleted its durable open
record ([RFC 14 §9.1](rfc-14-open-state.md#9.1%20An%20open%20keeps%20a%20file%20alive)) — open state reports, the service releases; when a release
commits that wrote the pending releases of its file's named streams
([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)), for each of those streams, which no open can hold since an open of
a stream counts as one of its base; and in recovery, for every pending release,
once the shard's grace has ended, read with `PendingReleases(share, shard)`
from the shard's pending-release index `S‖id‖pr‖shard‖file`, never by a scan
of the share's files
([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees), [RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)). The release transaction
re-checks its holders itself ([RFC 7 §4.5](rfc-7-namespace-metadata.md#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction)), so a release begun on a stale view aborts
and loses nothing. A delete on close runs as the principal of the open that set
it, through that open's reference — `Remove` passes its `via` to the metadata
store's `Unlink` ([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)) — so the open's grant — a backup or restore
intent's privilege included — authorises it (§4.6,
[RFC 7 §8.2](rfc-7-namespace-metadata.md#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later)). One still refused is logged at `Warn`, counted (§7), and
the file keeps its name: the client has closed, and no reply reaches it
([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)).

### 5.4 Remove and rename

The metadata store applies the namespace change. The final unlink always writes
the file's pending-release record in the same transaction, and holders are only
the durable open records; the release transaction deletes the record once no
open remains ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)), and the service runs it (§5.3). A crash between the
unlink and the release leaks nothing, because the record survives it and
recovery runs the release. A rename that replaces a target applies the same rule
to the target.

**A delete-pending directory takes no new entries.** Before it applies a create,
a link or a rename, the service **MUST** check the open state of the destination
parent and refuse the call if that directory is marked delete on close
([RFC 7 §8.2](rfc-7-namespace-metadata.md#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later)), whatever the protocol, as it refuses a new open of the directory.
The metadata store cannot see the mark, so no other layer checks it.

### 5.5 Size changes

A `SetAttr` that changes size, and a `Deallocate`, are authorised and checked
like a write, clear setuid and setgid as a write does (§5.1 step 5), then are
applied by the engine's `Truncate` or `Deallocate` at the file's primary; the other attributes of the same `SetAttr` are
applied by the metadata store in the same service call. A `SetAttr` that sets
`mtime` or `atime` to a given time is applied through an existence commit after
every write staged before it ([RFC 7 §9.5](rfc-7-namespace-metadata.md#9.5%20An%20explicit%20time%20outlives%20the%20writes%20staged%20before%20it)).

### 5.6 Quota

The service is the one enforcer of quotas. Neither the metadata store nor the
engine refuses a write for quota.

- At step 4 of a write, the primary adds the charge the write would add to an
  in-memory **reservation** per principal, per project and per share, and
  refuses with `ErrQuota` when, for any of them with a quota — a principal's
  being its own or else the share's default for its kind, the share's being
  the share-wide quota, whose usage is the sum of the share's per-shard usage
  counters — committed usage plus unfolded deltas
  ([RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)) plus its reservations would exceed the hard limit.
  The existence commit that records the charge releases the reservation; so
  does a write that fails. A reservation is volatile: after a crash the journal
  re-applies the writes and their charge ([RFC 8](rfc-8-engine.md)).
- **Overshoot bound.** A primary sees its own reservations, not other primaries'.
  Each primary lets a principal, project or share hold at most its **slack**
  reserved and uncommitted: `min(S, f × Hard.Bytes)`, where `S` and the
  fraction `f` are settings of [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings), so a small quota's slack is small; a write past it waits for a
  commit or returns `ErrDelay`. A principal writing through `k` primaries at once
  can therefore exceed its hard limit by at most `(k − 1) ×` its slack, plus what
  transactions committing concurrently add, and so can a project or a share
  writing through `k` primaries, under the same bound. With one primary, only
  the latter.
- **Soft limits.** A quota's soft limit and grace time
  ([RFC 16](rfc-16-metadata-store.md)) are enforced here too. Crossing the
  advisory threshold or the soft limit emits a quota event (§3.3) and refuses
  nothing. A grace that expires, a hard refusal and a fall back under a limit
  each emit one too; whether they refuse is the hard limit's rule above and
  the enforcement below. The metadata store records when the soft limit was first exceeded,
  and once the grace time has run from then the soft limit is enforced as the
  hard one. Falling back under it clears the record. The time is taken from
  the store's `Now` and compared with a later `Now`, which on a store whose
  `Now` is each node's own clamped clock may be another primary's, so the
  grace time ends up to the clock-offset bound σ early or late
  ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)); a quota's grace is not a fence and tolerates it.

### 5.7 GetAttr

The metadata store is a leaf: it never calls the engine. `GetAttr` joins
`Files.Get` with the engine's overlay — `Size`, `Times` and `Version` of writes
staged but not yet committed — at the file's primary, where both are local. The
overlay wins where it is newer. The two are read under the file's commit
serialisation, retried as a sequence lock, so a commit between them never yields
a lower size or `Version` than one already returned ([RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives)).

**The overlay's `Version` advances on every write the primary accepts**, at
acceptance, not at the existence commit that later records it. Each change draws
its `Version` from the journal's version counter, so it rises across restarts
and primaries too ([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)). A `GetAttr`
after a write is acknowledged returns a `Version` greater than any returned
before it. The existence commit **MUST** leave the committed `Version` no lower
than any the overlay reported for the writes it records, so the change attribute
never moves backward when the overlay drains. So an NFS client's close-to-open
check, and SMB change detection, see another client's staged write before it is
committed.

This is the only read path for a file's
attributes; `ReadDir` with attributes does the same join per entry, batched
per primary for entries whose files live in other shards, and so do `Lookup`,
`Create` and `SetAttr` before they return a File. NFSv3 pre-operation attributes
are returned only when the primary captured them and accepted the operation in one
hold of the file's accept lock — not under the commit serialisation, which orders
commits but lets another write be accepted between the capture and the operation —
and never for a directory; where the lock cannot be taken they are omitted
([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps), [§9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)).

### 5.8 The write verifier

The NFS write verifier is returned by the engine's `Write` and `Commit` and
derived from the primary's node, its node epoch, the process instance, the
shard's incarnation — raised each time a node or journal begins serving the
shard as primary
([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)) —
and the loss generation of the journal holding the file, sampled before the write
is staged; not from the shard epoch ([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state), [RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)). The service
passes it through unchanged. It therefore changes whenever the node serving the
file's shard as primary changes or restarts, whenever a shard comes back to a node
that served it before, and whenever the running primary's journal loses
unoffloaded writes it had acknowledged as unstable or fails a sync window
([RFC 1 §3.8](rfc-1-journal.md#3.8%20Loss%20events)); and not when only the shard epoch is raised. A change is what makes a client resend writes it sent unstable
([RFC 14 §10](rfc-14-open-state.md#10.%20Shard%20placement)); on the normal path none of the inputs moves, and the
verifier is constant.

**`Write` samples the verifier before staging; `Commit` after its sync.** A
`Write` returns the verifier sampled before the bytes are staged, so a loss that
takes the write shows in a later sample. A `Commit` returns the verifier sampled
once the sync it waits for has completed, so a loss before or during that sync —
a failed window included — makes the `Commit`'s verifier differ from the one its
writes returned, and the client resends them. Sampled before the sync, a window
that failed under the `Commit` would be answered with the old verifier and the
writes never resent.

**SMB learns through its flush.** SMB has no verifier. A `Commit` through an SMB
open (SMB `FLUSH`) takes the file's loss sequence from the engine's `Commit`
over the flushed extent, which returns it with the verifier, both read once the
sync completes ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)), and
passes it to open state, which fails the flush with `ErrLost` once for each open
that was open across a loss of an acknowledged, unoffloaded write of the file
([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens));
the SMB adapter maps it to `STATUS_UNEXPECTED_IO_ERROR`.

### 5.9 Copy and clone

`Copy` is one operation for NFSv4.2 `COPY` and `CLONE` and SMB's copychunk and
duplicate-extents requests, and runs as the engine's clone
([RFC 8 §9](rfc-8-engine.md#9.%20Clone)): a durable clone spec, the source's
journal-held runs copied and synced, then its carved refs adopted, all resolved
at one point in the source's history. The two kinds differ in atomicity:

- **`CLONE` and duplicate-extents are atomic.** The whole extent is one clone,
  answered only when it is done; a request longer than `clone_max_len`
  ([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)) is refused with `ErrInvalid`, so the client copies it itself.
- **`COPY` and copychunk are not atomic.** The engine runs them as bounded
  chunks, each a small atomic clone, and the reply reports the bytes copied; at
  the caller's deadline the copy stops after its current chunk and answers a
  short count (`wr_count` below the request, as RFC 7862 §15.2 allows), from which
  the client continues. A chunk refused after others are done answers the same
  short count; only a refusal of the first chunk returns its error
  ([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)).

1. admit and route both handles, and authorise read on the source and write on
   the destination, as §5.1 steps 1–3 do for each. **(cluster)** Between files
   of two shards, whatever their primaries, nothing is adopted by reference
   ([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)): every copy or clone between them is refused with
   `ErrCrossShard`, changing nothing, and for a `COPY` or copychunk the
   adapter then copies the bytes itself through `Read` and `Write` — reads at
   the source's primary, writes at the destination's, in bounded chunks with
   the same short-count rule (§4.3, §4.8);
2. refuse with `ErrInvalid` a copy whose source and destination are one file and
   whose extents overlap, as Linux and RFC 7862 refuse it, or a clone over
   `clone_max_len`; refuse with `ErrCrossNamespace` one RFC 8 §9.1's admission
   rule refuses — a destination share that does not list every namespace the
   source's share lists, or one whose re-home is `closing` from a source naming
   the namespace being left — since no chunk can be adopted across namespaces;
   nothing is changed. Refuse with `ErrIncomplete` one whose source File is
   `Incomplete`, before anything changes
   ([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)): a client copying the bytes itself would fail at its
   first read and change nothing, where a mark on the destination would make
   all of it unreadable;
3. reserve the destination's charge (§5.6), and clear setuid and setgid on the
   destination as a write does (§5.1 step 5);
4. call the engine. While a clone or a copy's current chunk runs, a write,
   truncate, deallocate or clone into its source extent, a write into its
   destination extent, and a truncate, deallocate or clone of its destination
   file or the destination's release, is **held** at the service until that
   clone or chunk ends and then runs, and returns `ErrDelay` (§4.3) only if its
   own deadline passes first, so each clone reads one consistent source and no
   removal of the destination races its batches
   ([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)). Answering
   `ErrDelay` at once would cost an NFSv3 client its fixed retry-later pause,
   several seconds on a common NFS client, for a chunk that ends far sooner.
   The call's context carries the adapter's **hold policy** (§4.3): a notifier
   the service calls once, when a hold begins, so an SMB adapter sends its
   interim response then (§3.2); the key under which the service counts holds
   across calls — the front-end node and an NFSv4.1 session or SMB connection
   on it, unique across the installation; and its budgets, in calls and in
   bytes. The service counts every call that carries a policy, from its first
   hold or `ErrDelay` — behind a clone, a recall, a freeze, capacity, a
   stalled store or any other cause — until its context ends, whether the
   service or the adapter holds it at the time; the holds this step names are
   one case of it. An NFSv4.1 session's budget is a quarter of its slots, and
   at least 1, since a held call keeps its slot, and is a hard cap; NFSv3,
   NFSv4.0 and NLM pass no policy, and the call's deadline alone bounds their
   holds; SMB passes its connection as the key, with the per-connection
   figures RFC 22 states as a floor (`Pooled`). **Each node that counts holds
   keeps one hold pool**, which takes what pooled keys hold past their floors:
   a sixteenth of the process memory limit (RFC 24 §2.1) — derived, not a
   setting; a test
   sizes it by setting that limit, 128 MiB giving an 8 MiB pool. A call
   drawn on it is charged its whole receive reservation, buffer class and
   overhead (RFC 24 §4.3); the per-key byte budget counts its write's bytes,
   and at least 64 KiB, one SMB credit's size, so the pool bounds held calls
   as well as bytes. A call that
   would pass a hard budget, or a pooled key's floor while the pool cannot take
   it, is answered `ErrDelay` with `DelayHoldBudget` at once, holds nothing,
   and is counted as a refusal (§7).

   > ponytail: the pool is first come, first served past the floors, so one
   > key's burst can take all of it and leave another key refused past its
   > floor while that one holds gigabytes. On a 64 GiB node it is 4 GiB: a
   > multi-session host with 40 users at queue depth 8 of 1 MiB writes holds
   > 320 MiB through a freeze. Cap each key's share of the pool when
   > `vfs_hold_refusals_total` counts refusals on keys other than the one
   > holding most of it.

   > ponytail: **(cluster)** each primary counts the holds it makes on its
   > own, so a session or connection whose calls reach `n` primaries can hold
   > up to `n` times its budget. Count at the front-end instead, the primary
   > asking it before a hold, when a cluster shows sessions running out of
   > slots or connections past their byte cap.

   A held SMB write whose own deadline passes first is answered `STATUS_FILE_LOCK_CONFLICT`, as for a range another
   holds — unless the clone it waits on is itself held up by a stalled metadata
   store, a remote outage, or a restart's wait for space
   ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing), step 4), when the call is answered with that cause
   (`DelayStoreStall`, `ErrUnavailable` or `DelayCapacity`), not `DelayClone`,
   and the adapter holds it and answers as for that cause (§3.2). An open of a
   destination a restart has not yet served answers `ErrDelay`, with
   `DelayCapacity` while its clone waits for space and `DelayClone` otherwise.
   The clone holds the source open, so a last close or unlink does not release
   it mid-clone. A source run the engine reads as Lost — a
   source ref whose chunk record is gone included — is carried to the
   destination as Lost, never as zeros. A clone that is refused leaves the
   destination's prior content; once begun it is not undone but resumed after a
   crash from its durable spec, and is answered only when done.

## 6. Invariants

| # | Invariant |
| --- | --- |
| V1 | Adapters hold only `vfs.Service`. No adapter holds a metadata view, an engine, open state or a component. |
| V2 | Every client operation that needs authorisation reaches `Files.Authorize` exactly through the service. |
| V3 | Every cross-component order of a client operation is the one in §5, in every protocol. |
| V4 | A routing refusal never reaches an adapter. |
| V5 | The service persists nothing and imports no adapter. |
| V6 | A recall or break that is not acknowledged by its deadline revokes the grant; no worker waits on a recall — the conflicting operation returns `ErrDelay`. |
| V7 | The conflict check and the I/O it admits run at one primary, and no conflicting state is granted between them. |
| V8 | Every open-state call names its `ClientID`, and an ID the client does not hold is refused. |
| V9 | The share grant is evaluated on every call. |
| V10 | Quota is enforced only by the service, and overshoot stays within the bound of §5.6. |
| V11 | (cluster, [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)) A forwarded mutation retried after a lost reply returns its original result and applies once. |
| V12 | Every call that names a handle or an open is admitted to its share — state, flavour, client rules, squash — whatever path the handle arrived by. |
| V13 | No mount, tree connect or session is stored; a reconnecting client is admitted afresh. |
| V14 | Every node builds the same pseudo-filesystem from the same share list: the same handles, numeric ids, `fsid` and change attribute. |
| V15 | No adapter resolves a share name or path, admits a call, or builds or interprets any part of the pseudo-filesystem; all of it is behind `Shares` and `Names`. |
| V16 | The `Version` `GetAttr` returns advances on every write the primary accepts, and never moves backward when the write is committed, across restarts or across primaries; `GetAttr` reads the File and the overlay under the file's commit serialisation. |
| V17 | Every release is run by the service, at the file's primary, after an orphaning unlink or rename, at the last close of a file with no entry, for each named stream a committed release left pending, and in recovery after grace. |
| V18 | An open's grant authorises a call only for the principal it was granted to; a call through an open, lock state or layout by any other principal is refused with `ErrNotYours`. |
| V19 | A write through a persistent open, or any open on a continuously available share, is stable; a stable write is answered after the journal sync and, if it overwrites committed content, after its existence commit; a stable append or hole fill waits for no metadata transaction. |
| V20 | A refusal by a share's journal limit or the journal's capacity returns `ErrDelay` while offload or repack can free space, and a deadline running out never turns it into `ErrNoSpace`; an SMB request held for it is answered `STATUS_DISK_FULL` only at `smb_pending_cap`. `ErrNoSpace` is answered at once and only for a permanent cause; `ErrQuota` is answered at once. |
| V21 | A deny-mode refusal of any request — an open of either protocol, anonymous or delegation I/O, a remove or rename — is decided only after the handle-caching grants covering the conflicting opens are broken. |
| V22 | An unprivileged caller's write, truncate, deallocate, copy or clone into a setuid or setgid file — the owner's included — clears the bits in a committed change before the bytes are staged. |
| V23 | A blocked lock holds no worker and has no deadline: NFSv4 waiters are polled, NLM and SMB waiters are answered through `LockWaitOver`, and no adapter answers one other than as granted before `CancelLock` returns. |
| V24 | I/O under a delegation is served only to the client holding it and is authorised for the caller's own principal, as anonymous I/O is. |
| V25 | `Write` samples the verifier before staging and `Commit` after its sync; an SMB flush through an open that was open across a loss of the file's acknowledged unoffloaded writes fails once with `ErrLost`. |
| V26 | A copy or clone of overlapping extents within one file, or a clone over `clone_max_len`, is refused with `ErrInvalid`, one across namespaces with `ErrCrossNamespace`, one from an `Incomplete` source with `ErrIncomplete`, and **(cluster)** any one across shards with `ErrCrossShard`, each changing nothing, after which the adapter copies a `COPY` or copychunk's bytes itself; writes to a running clone's extents, and removals of its destination, are held until it ends, and return `ErrDelay` only past their own deadline; a Lost source run is copied as Lost; a clone is atomic and answered only when done, and a copy runs as bounded chunks and may answer a short count. |
| V27 | A request an adapter holds pending after `ErrGrace` outlives the grace period, and one held while its share is quiesced or frozen outlives the freeze, bounded by `migration.freeze_timeout` plus the break deadline (§3.2). |
| V28 | A create, link or rename into a directory marked delete on close is refused, over every protocol. |
| V29 | Every call takes the open-state steps of the files it touches, then fold admission, then accept locks, each set in file-ID order; a call queued for fold admission holds no accept lock, and a held call holds none of them and re-runs §5.1's steps 3–6 when its hold ends. |
| V30 | Every call carrying a hold policy is counted from its first hold or `ErrDelay` until its context ends; past a hard budget, or past a pooled floor with the node's hold pool full, it is answered `ErrDelay` with `DelayHoldBudget` at once and holds nothing. |

## 7. Observability

Metric names are shown without the deployment's prefix.

| Answers | Metric | Type |
| --- | --- | --- |
| How fast is each operation, per protocol? `outcome` is `ok`, `error` or `delay` | `vfs_op_seconds{op, protocol, outcome}` | histogram |
| Which errors does each operation answer? | `vfs_errors_total{op, error}`, `error` one of §4.3's neutral errors | counter |
| How often are clients told to retry? | `vfs_delayed_total{op, reason}` | counter |
| How often does quota refuse, and how much is reserved? | `vfs_quota_refusals_total{kind}`, `vfs_quota_reserved_bytes` | counter, gauge |
| Are callbacks failing? | `vfs_callback_errors_total{kind}` | counter |
| What is held, and how often is a call refused past its hold budget (§5.9)? | `vfs_held_calls`, `vfs_held_bytes`, `vfs_hold_pool_bytes`, `vfs_hold_refusals_total{protocol}` | gauge, gauge, gauge, counter |
| How often is a delete on close refused after its client closed (§5.3)? | `vfs_deferred_unlink_refusals_total` | counter |

Operation latency and quota metrics are owned here and nowhere else. Events
dropped are counted at the sink, RFC 25's `events_dropped_total`, not here.
Forwarding and epoch refusals are RFC 15's; recalls are RFC 14's.

Each service call opens a trace span that parents the metadata store's and the
engine's spans. No share label on per-operation metrics ([RFC 16 §8.1](rfc-16-metadata-store.md#8.1%20Metrics)).

## 8. Conformance

- **Service suite:** drives `vfs.Service` directly with a fake `Callbacks` that
  records what adapters would have been told. It covers every operation's
  order (§5), and the cross-protocol cases: an NFS lock against an SMB deny
  mode, an NFS write against an SMB lease, a recall that times out, a client
  whose lease expires holding state of both kinds. It also asserts:
  - a conflicting operation returns `ErrDelay` while its recall is pending,
    with no worker blocked;
  - an `OpenID`, `GrantID` or `WatchID` presented by another client is refused;
  - a client removed from a share grant is refused on its next call with a
    handle it already holds;
  - with a directory marked delete on close over SMB, a create, a link and a
    rename into it, over SMB and over NFS, are each refused (V28);
  - a recursive watch reports nothing below a directory its holder cannot
    traverse;
  - during a shard's grace, a write overlapping a reclaimable lock returns
    `ErrGrace`;
  - a lock granted concurrently with a write is ordered wholly before or after
    it (V7), driven by a model test that interleaves the two;
  - client A reads `GetAttr`'s `Version`, client B writes and is acknowledged,
    and A's next `GetAttr` returns a greater `Version` before any existence
    commit; after the commit it is not lower (V16);
  - an SMB call that meets a pending recall gets an interim response and
    completes once the recall ends, without the client resending it, and a
    recall that outlasts the call's deadline completes it with the deadline's
    error (§3.2);
  - **(cluster)** a forwarded write retried after a dropped reply, with another
    write between, applies once (V11);
  - the verifier `Write` and `Commit` return through the service changes after
    the primary's journal drops an unstable write as lost, in the same process
    and epoch, and when a shard is handed away and back to the same process, and
    is unchanged across a shard-epoch raise that moves nothing (§5.8); a service
    that caches the verifier per process passes the last and fails the others;
  - writers through `k` primaries overshoot a hard limit by no more than §5.6's
    bound, counted per run, on a 1 GiB project quota as on a 1 TiB one: an
    installation slack applied to the small quota unscaled fails it;
  - a write refused by a share-wide, project, user and group quota in turn
    returns a `*QuotaError` naming each scope, and with two of them exceeded
    names the first in §4.3's order;
  - `StatFS` on a handle inside a project with a quota tighter than the
    share's reports the project's limit and usage, and the share's once the
    project's quota is deleted;
  - an event sink that blocks does not slow an operation, and its drops are
    counted;
  - for each of lock, unlock, test-lock, layout-get and watch, reached both by
    a handle presented directly and by a walk from the pseudo-filesystem: a
    quiesced share returns `ErrDelay`, a share whose policy refuses the flavour
    returns `ErrWrongSecurity`, and a client outside its client rules returns
    `ErrAccess` (V12);
  - a compound crossing from a share squashing root into one that does not acts
    as root only in the second;
  - a renamed share keeps serving held handles and established tree connects,
    and a new tree connect needs the new name;
  - two services built from the same share list return byte-identical pseudo
    handles, numeric ids and change attributes, and a share added on one is
    seen on the other with a raised change attribute (V14);
  - an unlinked file whose last open closes is released, and so, after a
    restart, is a file whose pending release was written before the crash; a
    service that never calls `Release` leaks both (V17);
  - bob presents alice's open to write a file alice may write and bob may not,
    and again to write a file both may write; assert `ErrNotYours` both times
    and alice's open intact. A service that trusts the open's grant for any
    presenter accepts the first write; one that re-authorises bob afresh accepts
    the second (V18);
  - an NLM and an SMB lock that wait are answered `ErrBlocked` at once and later
    granted through `LockWaitOver` when the holder unlocks, with no service
    worker held between; an NFSv4 one is answered `ErrLocked`, and its next poll
    after the unlock is granted (V23);
  - an SMB lease whose break times out leaves its open usable: the holder's next
    write through it is accepted (§3.2);
  - a write on a persistent open, asked unstable, survives a crash injected right
    after its reply; a stable append's reply waits for the journal sync and for no
    metadata transaction, counted by the store's transaction counter; a stable
    overwrite of committed content is not answered until its existence commit has
    landed — stall the store, and assert the reply waits and then answers
    `ErrDelay` at the deadline rather than success (V19);
  - with the journal full of dirty bytes and the remote reachable, a write
    returns `ErrDelay`, and succeeds on retry once offload and repack have freed
    space (V20);
  - an SMB open that conflicts on deny mode with an open a handle-caching lease
    covers gets a break of that lease first; when the lease's client closes the
    handle, the open succeeds without the client resending it (V21);
  - bob writes alice's setuid file; `GetAttr` read between the write's reply and
    any commit shows the bit clear. Repeat with alice, unprivileged, as the
    writer, then with a deallocate, a copy and a clone into the file: assert the
    bit clear each time. A service that clears only for non-owners, or only on
    writes, fails one (V22);
  - an NFSv4.1 client holding a write delegation writes under its stateid as a
    second principal that may write the file, then as a third that may not:
    assert the first accepted and the second `ErrAccess`; another client
    presenting the stateid gets `ErrNotYours` (V24);
  - a `Commit` that waits on a sync window that fails returns a verifier other
    than its writes'; an SMB flush through each of two opens of a file that lost
    an unstable write in the running process fails once and then succeeds (V25);
  - a copy and a clone of overlapping extents within one file each return
    `ErrInvalid` and leave the file unchanged; a write to the source extent of a
    running clone is held and completes once the clone ends, and returns
    `ErrDelay` only when its own deadline passes first; a clone of a source with a Lost run reads
    Lost there at the destination, never zeros; a `CLONE` over `clone_max_len`
    returns `ErrInvalid`, and a copy or clone between shares of two namespaces
    returns `ErrCrossNamespace`, each changing nothing; a truncate of the
    destination during a clone is held until the clone ends; a `COPY` whose deadline
    passes partway answers a short count, and the destination holds exactly
    that many bytes of the source (V26);
  - with a share at its journal limit and offload able to drain it, a write
    returns `ErrDelay` and succeeds on retry; with the store unreachable past the
    caller's deadline, an NFS write is still answered `ErrDelay`, never
    `ErrNoSpace`, and an SMB write held pending is answered `STATUS_DISK_FULL`
    only once `smb_pending_cap` has passed; with the store refusing puts as
    denied, the write answers `ErrNoSpace` at once (V20);
  - an NFSv4 `OPEN` and an NFSv3 `WRITE` that meet a deny mode held only in an
    SMB handle cache each get `ErrDelay`, the lease is broken, and the retry
    succeeds (V21);
  - an SMB lock that waits five minutes is still pending and is granted on
    unlock; an SMB cancel raced against the unlock leaves either a granted reply
    and a held lock or a cancelled reply and none (V23);
  - an SMB open of a new file during a grace period that lasts its full lease
    period is held pending and granted when grace ends; an SMB write to a share
    frozen for two minutes by a move is held pending and completes when the
    freeze ends, not failed at `smb_pending_deadline` (V27);
  - a pending SMB request whose recall takes the full break deadline completes
    rather than failing at the 30 s default deadline (§3.2);
  - 16 readers `GetAttr` a file while its writes commit in a loop; none sees a
    `Version` lower than one it saw before (V16).
  - each `ErrDelay` the service returns — a recall, a held clone, a freeze, a
    moved namespace, capacity, the version limit, a stalled store, quota slack,
    a conflict still unresolved at the deadline — carries its `DelayCause`, and
    a freeze's carries its bound; an SMB write held on a freeze completes at the
    freeze's end, one held on capacity is answered `STATUS_DISK_FULL` only at
    `smb_pending_cap`, one held on the version limit or quota slack
    `STATUS_UNEXPECTED_IO_ERROR` only there, and one held on a conflict
    `STATUS_FILE_NOT_AVAILABLE` only there; one past its hold budget carries
    `DelayHoldBudget` and is answered `STATUS_INSUFFICIENT_RESOURCES` at once,
    never held (§3.2, §4.3);
  - with an SMB write held pending on a share, remove the share: assert the
    fake `Callbacks` receives `Revoked` with `RevokedShareRemoved` naming it, the
    held write is answered, and the removal's step 3 waits for a create admitted
    before the mark (§4.9);
  - an SMB read of an evicted extent during a 90 s remote outage is held and
    completes once the store returns; a read of a **Lost** extent fails at once
    (§5.2);
  - a recall that misses its deadline delivers `RevokedGrant` naming that
    grant, `Renew` reports `RecallableRevoked`, and a write under its stateid
    returns `ErrRevoked`, not `ErrNotYours` (§3.2);
  - an unprivileged append to a setuid, append-only file succeeds and clears the
    bit; a write below its size is refused `ErrPerm` and clears nothing (§5.1);
  - over SMB multichannel, losing one channel of a session leaves its opens
    as they were; losing its last channel disconnects only that session's
    opens (§3.1);
  - restart the process with an NFSv4.0 client holding a lock: assert its
    stateid answers `ErrStaleState` during grace and the lock is reclaimed;
    after grace, a stateid its client freed answers `ErrBadState` (§4.3);
  - an administrator revokes a client holding an open, a lock and a delegation:
    assert each stateid answers `ErrRevoked`, `Renew` reports `AdminRevoked`
    only, never `RecallableRevoked`, its record and sessions remain, and after
    `FreeRevoked` of each both clear and it opens afresh under the same client
    ID; repeat over NFSv4.0 and SMB, where the adapter calls `FreeRevoked` after
    answering each stateid revoked once (§3.2);
  - a `chmod` draws its version, a write is accepted and committed at a higher
    one before the `chmod` commits: assert the `chmod` is redrawn and stored
    above the write, and `GetAttr`'s `Version` never moves backward; repeat
    with the write staged and not yet committed when the `chmod` commits (§5.1);
  - a write into a file and an unlink of the file's last name, racing 10⁴ times
    with an SMB deny-delete open on it: assert neither waits for its deadline
    and no deny-delete is granted between the unlink's check and its commit
    (§5.1's lock order);
  - an SMB `MAXIMUM_ALLOWED` open of a file whose parent grants delete-child
    includes `DELETE`; change the parent's group so it no longer does, and
    assert a cached delete-child decision is not reused (§4.6);
  - a backup-intent handle of a privileged caller sets owner, group and DACL on
    a file whose DACL excludes the caller, as `robocopy /B /SECFIX` does: assert
    all three succeed; the same caller without backup intent is refused; a
    rename through a handle opened with `DELETE` succeeds where the caller
    lacks delete-child on the directory (§4.6);
  - an NFSv3 write into a cloning extent is held and completes when the clone
    ends, never `ErrDelay` at once; with an NFSv4.1 session of 2 slots, the
    first held call is held and a second is answered `ErrDelay` with
    `DelayHoldBudget` at once; with an SMB policy of 32 MiB and the hold pool
    at 8 MiB through a 128 MiB process memory limit, six held 8 MiB writes on one connection leave the fifth
    held on the pool and the sixth answered `DelayHoldBudget` at once and
    counted; an SMB write that meets `ErrDelay` for capacity, and that the
    adapter holds and re-drives under the same context, counts against the
    connection's budget for its whole hold — a count of clone holds alone
    lets every such write through (§5.9, V30);
  - clone a 1 GiB extent of a VM disk while the VM writes into it and another
    client's SMB lease on the file has a break pending: assert each held write
    gives up the file's open-state step and accept lock for its hold, the
    break's acknowledgement and a new open of the file proceed during the
    clone, the clone's last batch deletes its clone-kind open without waiting
    on a held write, and each held write then completes after re-running its
    checks. A service that keeps the open-state step across the hold waits
    until the write's deadline and revokes the lease (§5.1);
  - copy and clone from a file a restored snapshot lists as incomplete into an
    existing 2 GiB file: assert `ErrIncomplete` for each, and the destination
    unchanged and readable (§5.9);
  - a privileged caller's backup-intent open with delete on close of a file
    whose DACL excludes it, as `robocopy /B` purging extras does, and an EA set
    through the same handle: assert both succeed. Mark the parent immutable
    before the close: assert the deferred unlink is refused, counted in
    `vfs_deferred_unlink_refusals_total`, and the file keeps its name (§5.3);
  - with a waiting SMB lock and an outstanding `CHANGE_NOTIFY` on a share, move
    the share's namespace away: assert the fake `Callbacks` receives `Revoked`
    with `RevokedMoved` naming it and the move's address, the lock and the
    notify are answered, and the share's opens and deny modes are gone at the
    old owner; assert an NFS call on that address gets no reply, every SMB
    connection on it is closed and a new one refused, a non-CA tree of the
    share reached through another address is ended
    `STATUS_NETWORK_NAME_DELETED`, and a connection carrying a CA share's tree
    through another address is closed (§3.2).
- **Import test:** no adapter package imports the metadata store, open state or
  the engine; the service imports no adapter (V1, V5). The `Shares` calls and
  pseudo handles are driven in the service suite with no adapter at all, and
  every case above runs once per entry call (`Mount`, `PseudoRoot` + `Lookup`,
  `TreeConnect`) (V15).
- **Split run:** stated once in [RFC 15](rfc-15-topology.md), gated on the first remote view
  shipping.
- **Benchmarks:** each operation through the service, collocated and split, so a
  protocol benchmark minus a service benchmark is the adapter's own cost.

## 9. Open questions

1. **Compound batching.** Adapters run compounds as sequences of calls. Whether
   a batch call (a `LOOKUP`-`GETATTR`-`OPEN` chain in one service call) pays for
   itself under a split deployment is unmeasured.
2. **Where the pNFS layout operations live.** `LAYOUTGET` and `LAYOUTCOMMIT` are
   protocol-specific but cross primaries ([RFC 15](rfc-15-topology.md)); whether they are service
   operations or an adapter-side protocol over `Data` is open. Either way a
   layout is bound to the (shard, epoch) it was granted under ([RFC 14](rfc-14-open-state.md)).

## Appendix A — prior art

*Non-normative.* The layer exists under different names in every
multi-protocol file server that kept its protocols consistent:

- **Samba's VFS layer** — every SMB operation passes through a stack of VFS
  modules before the file system; protocol code never touches the backend.
- **NFS-Ganesha's FSAL** — the protocol layers call one file-system abstraction
  layer, with a backend per storage system.
- **The Linux VFS** — system calls, the in-kernel NFS server and the SMB server
  all reach file systems through the same inode and file operations.
- **libcephfs** — one client library that NFS-Ganesha and Samba both call, so
  both protocols see one set of capabilities and locks.

The shared lesson: the protocol layer translates, and one layer beneath it
orders and enforces.
