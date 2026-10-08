---
rfc: 7
title: "RFC 7 — namespace metadata"
component: namespace metadata
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-16-metadata-store]]"
aliases:
  - RFC 7
tags:
  - rfc
---
# RFC 7 — namespace metadata

**Status:** draft.
**Audience:** anyone changing the namespace entities, the handle format or the
permission path — and anyone writing the filesystem service ([RFC 17](rfc-17-vfs.md)) that
consumes them.

[RFC 6](rfc-6-block-metadata.md) owns the records that describe a file's content; this document owns the
entities that describe the file. [RFC 16](rfc-16-metadata-store.md) owns how every metadata entity is
persisted — the store contract, key layout and codecs — and [RFC 14](rfc-14-open-state.md) owns open
state and locks. Conventions, RFC 2119 keywords and test tiers are set once in
the [index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists where the code differs, as defects to fix, never rules to
build around.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

This RFC specifies the part of the metadata that clients see as a filesystem:
names, directories, files and their attributes, the handles clients hold, and
who may do what. It also decides the moment a file stops existing, which is the
moment its content may start to be destroyed.

In outline: clients write to DittoFS over NFS (Network File System) or SMB
(Server Message Block); writes land in a local journal and are later cut into
chunks, packed into blocks and uploaded to an S3 (Simple Storage Service)
bucket; a metadata store records which file holds which chunks
([RFC 0](rfc-0-data-lifecycle.md)). Content is keyed by a file's identity,
never by its name. This RFC owns the mapping from names to that identity.

### The problem, in one example

The `profiles` share is served over SMB to Windows desktops and is configured
case-insensitive, as Windows expects. alice's folder `profiles/alice/` holds
her profile disk `ODFC_alice.vhdx`, 30 GB.

1. **Sign-in.** alice-pc opens `alice\odfc_alice.VHDX`. The name is resolved
   one directory at a time, with a permission check at each. In the share's
   root, the name `alice` is folded to one case and looked up: one read finds
   the entry, a second reads the directory it names. In that directory,
   `odfc_alice.VHDX` folds to the same key as the stored `ODFC_alice.vhdx`, so
   one more lookup finds the entry and the file it names. The name is not
   changed: a listing still shows `ODFC_alice.vhdx`.
2. **The handle.** alice-pc is given a handle naming the share and the file's
   identity, a random 128-bit ID that is never reused. All day, every 64 KiB
   write goes by handle, and resolving it reads the file directly: no name, no
   directory.
3. **A rename.** At noon an administrator renames the folder to `alice.old`.
   That moves one entry, in one transaction, and nothing else. A handle built
   from the path would now point at nothing, and alice's open disk would fail
   mid-session. The handle names the file, so alice-pc keeps writing.
4. **A temporary file.** alice-pc opens `~tmp1.dat` twice, the first time with
   delete-on-close, and closes that open first. The file becomes *delete
   pending*: its name still shows in a listing, but new opens are refused. When
   the second open closes, the name is removed by an ordinary unlink. That unlink drops the file's link count to
   zero and, in the same transaction, writes a **pending release**. No open
   holds the file, so the release runs: its content references are dropped and
   its records deleted.
5. **A crash.** Had N1 crashed between that unlink and the release, nothing
   would be leaked: recovery finds the pending release and runs it once no
   open can still claim the file.

Without these rules the same day goes wrong quietly: a path inside the handle
breaks alice's disk on the rename; case folded differently by two protocols
shows one file as two; an unlink acknowledged but recorded nowhere leaks
every chunk of the file, because nothing is left to find them by.

```text
 alice-pc ── open "alice\odfc_alice.VHDX" on share profiles
   │
   │  Lookup(root, "alice")              entry  root / alice ──► dir d1
   │  Lookup(d1, "odfc_alice.VHDX")
   │      key fold("odfc_alice.VHDX")    entry  d1 / odfc_alice.vhdx
   │      value "ODFC_alice.vhdx"               │
   ▼                                            ▼
 handle = (profiles, f7) ───────────────► File f7: regular, Nlink 1,
   every write resolves f7 directly        Size 30 GB, owner alice
   no name, no directory read

 rename alice → alice.old: one entry moves; f7 and its handle do not
 last name of a file removed: Nlink 0 + pending release, same transaction
   no open holds it ──► release: refs dropped, records deleted
```

### The words you need

- **[File](rfc-0-data-lifecycle.md#Glossary)**: one object a name can resolve
  to, with its attributes; its identity is a random ID, never reused, that
  survives rename ([§2.1](#2.1%20File)).
- **Entry**: one name in one directory, pointing at a file. A file with two hard
  links has two entries ([§2.2](#2.2%20Entry)).
- **Handle**: what a client holds to name a file: the share and the file's ID,
  never a path ([§6.1](#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)).
- **Nlink**: the number of entries naming a file, kept exact in the same
  transaction as every entry change ([§4.1](#4.1%20%60nlink%60%20is%20exactly%20its%20entries)).
- **Open state**: who holds a file open, owned by
  [RFC 14](rfc-14-open-state.md). It is the only other thing that keeps a file
  alive ([§4.2](#4.2%20Open%20state%20is%20the%20second%20holder)).
- **Pending release**: a record saying a file has lost its last name and its
  content is still to be released. It keeps nothing alive
  ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)).
- **Folded name**: on a case-insensitive share, an entry is keyed by its name
  folded to one case, and stores the name as given ([§3.3](#3.3%20Case)).

### What this RFC promises

- A handle keeps working across rename and restart. Once its file is gone it
  resolves as stale, never as another file and never as "not found".
- A file is released when, and only when, no name and no open refer to it. The
  release is recorded before it runs, so a crash resumes it.
- A rename happens wholly or not at all, and no directory can end up inside
  itself, even when two renames race.
- A name comes back exactly as it was given. On a case-insensitive share, names
  that differ only in case are one entry, found with one read.
- Every permission decision, for every protocol, is made in this component,
  against the file the operation will act on.

### How the rest is organised

[§1](#1.%20Purpose) states what this component answers and what it must not
hold. [§2](#2.%20The%20entities) defines the entities: file, entry, ACL
(access-control list), extended attributes and streams, the share as one
filesystem with its quotas. [§3](#3.%20Names) covers names: lookup, validation,
case and listings. [§4](#4.%20What%20keeps%20a%20file%20alive) is what keeps a
file alive and how it is released; [§5](#5.%20Rename) is rename;
[§6](#6.%20Handles) handles; [§7](#7.%20Permissions) permissions.
[§8](#8.%20Open%20state%2C%20as%20the%20namespace%20sees%20it) and
[§9](#9.%20Attributes%20and%20what%20is%20not%20one) cover open state and
attributes as this component sees them; [§10](#10.%20Invariants) lists the
invariants. On a first read, skip §2.8–§2.10, §3.5–§3.8, §5.5, §6.5, §7.5–§7.8,
§9.4–§9.6 and §11 onward.

## 1. Purpose

Namespace metadata answers, for any client operation:

> **What entries exist, what are they called, which file does a name resolve
> to, and may this caller do this?**

Everything a client can name, it names through this component. It is the only
component that knows about paths, and it is the only component that decides
whether an operation is allowed to happen at all.

It is also the component that decides when a file stops existing, and records
that decision as a pending release; the release that drops its content is the
engine's `Release` ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)). Nothing else in the set may make that
decision, and nothing else may keep content alive behind this component's back
([§4.4](#4.4%20There%20is%20no%20third%20holder)).

**This component is a leaf.** It calls no other component and declares no
interface on one: the store holds its records and answers for them, and every
join with content or open state — `GETATTR`'s overlay, the release, `SEEK_HOLE`
— is made by the filesystem service ([RFC 17](rfc-17-vfs.md)) at the file's primary.

Every entity here is **protocol-neutral**. NFS and SMB vocabulary — uid, SID,
DOS attributes, security descriptors, `fsid`, volume serials — is translated by
the adapters and the filesystem service ([RFC 17](rfc-17-vfs.md)); nothing in this document is
shaped by one protocol.

### 1.1 Non-goals

Namespace metadata **MUST NOT**:

- hold content bytes, or know where they are — local placement is [RFC 1](rfc-1-journal.md)'s,
  residency is computed and never stored ([RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function));
- hold chunks, refs, blocks or refcounts — those are [RFC 6](rfc-6-block-metadata.md)'s, and this component
  never reads them: the filesystem service joins the two ([§2.5](#2.5%20Where%20%60size%60%20lives), [§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees));
- hold opens, locks, caching grants or watches — those are [RFC 14](rfc-14-open-state.md)'s; this
  component only asks whether an open keeps a file alive ([§8](#8.%20Open%20state%2C%20as%20the%20namespace%20sees%20it));
- decide what to offload, evict or sweep;
- encode or decode a wire protocol. A handle is opaque at this boundary and
  *stays* opaque above it ([§6](#6.%20Handles));
- choose keys or encodings — that is [RFC 16](rfc-16-metadata-store.md)'s;
- import another component in this set ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)).

### 1.2 Why it is a separate RFC from block metadata

[RFC 6 §1.2](rfc-6-block-metadata.md#1.2%20Why%20it%20is%20a%20separate%20RFC%20from%20the%20namespace) gives the reason and the table: the two are usually one database,
often one transaction, and they are specified apart because their write patterns
differ in kind. This document does not restate it.

The consequence that matters here is directional. Block metadata **MUST NOT** be
told about names ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)); it learns of a deletion only when this component
releases a file. So every rule below about what keeps a file alive is also a
rule about when content may be destroyed.

## 2. The entities

The namespace is specified as Go entities: plain structs a
caller reads whole. How many records hold each one, and under which keys, is
[RFC 16](rfc-16-metadata-store.md)'s choice and invisible here. What a *write* touches is decided by the
operation, never by the struct: no interface offers `Put(File)`, because a
caller that could write the whole struct would read-modify-write it and conflict
with every change to fields it never meant to touch.

| Entity | Identified by | Holds | Written by |
| --- | --- | --- | --- |
| **File** ([§2.1](#2.1%20File)) | `FileID` | type, owner, group, mode, flags, times, version, `Nlink`, parent (directories), size, charged bytes, applied version and write times (write path) | attribute operations, link and unlink ([§4](#4.%20What%20keeps%20a%20file%20alive)); size, charge, applied version and write times only by an existence commit ([§2.5](#2.5%20Where%20%60size%60%20lives)) |
| **Entry** ([§2.2](#2.2%20Entry)) | `(parent FileID, name)` — the folded name on a case-insensitive share ([§3.3](#3.3%20Case)) | the name as given, child `FileID`, child type | create, link, unlink, rename ([§3](#3.%20Names), [§5](#5.%20Rename)) |
| **ACL** ([§2.6](#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)) | `FileID` | the file's one access-control list | set-ACL, and `chmod` on a file that has one |
| **Xattr** ([§2.7](#2.7%20Extended%20attributes%20and%20named%20streams)) | `(FileID, name)` | one small named value | set and remove xattr |
| **Pending release** | `FileID` | nothing but its existence | the unlink or rename that removes a file's last entry; deleted only by the release transaction ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)) |

A named stream is a `File` ([§2.7](#2.7%20Extended%20attributes%20and%20named%20streams)); a share's root is a `File`, and its capacity
and quotas are `FilesystemInfo`, `Usage` and `Quota` ([§2.8](#2.8%20A%20share%20is%20one%20filesystem)).

![A directory's entries as separate records pointing at files, two entries naming one file, and the operations that write each side](img/rfc5-entries-and-files.svg)

### 2.1 File

One struct for every object a name can resolve to: regular file, directory,
symlink, device, FIFO, socket, and a named stream.

```go
type File struct {
	ID    FileID // RFC 0 §3: a UUID, never reissued, stable across rename and relink
	Share ShareID
	Type  FileType

	// Owner and permissions. Mode agrees with the ACL when one exists (§2.6).
	Owner, Group Principal
	Mode         uint32    // permission bits only; the type is Type
	Flags        FileFlags // hidden, system, archive, read-only, immutable, append-only, …

	// Times. Birth is set once at create; Change advances on every metadata
	// or content change; Modify only on content change (§9.2).
	Birth, Access, Modify, Change time.Time

	// Version increments on every change to the file, metadata or content:
	// every existence commit advances it, as every attribute change does.
	// It is the NFSv4 change attribute and what SMB change detection compares.
	// It is not a journal version: those order content writes inside one
	// journal (RFC 1 §5.3).
	Version uint64

	// Size, Charged, Applied, Modify and the Change a write sets are owned by
	// the write path: only an existence commit sets them (§2.5, RFC 6 §3).
	Size int64

	// Charged is the bytes this file charges to Owner, Group and Project
	// (§2.8): its logical bytes, holes excluded, in full whether or not its
	// chunks are shared. The existence commit that moves Size or the holes
	// moves Charged with them, so no charge is ever computed by a hole scan.
	Charged int64

	// Applied is the newest journal version whose existence change this File
	// records (RFC 6 §2.4); it only moves forward.
	Applied uint64

	// Nlink is the number of entries naming this file: its hard links (§4.1).
	// A symlink pointing at the file is not a link to it and is not counted.
	Nlink uint32

	Parent FileID // Type Directory only: the directory holding its one entry, so ".." needs no search

	// Shard is the group of files one primary serves at a time
	// (RFC 11). Zero, the default, means the whole share is one shard. Set at
	// create; unchanged by rename; changed only by a move between shards (RFC 11
	// §4), which rewrites it in bounded batches, never in one transaction. A
	// per-child or range shard (RFC 11 §2.2, Appendix C) is created the same way.
	Shard ShardID

	// Project is the tree quota this file is charged to (§2.8): inherited
	// from the parent at create. Zero: none.
	Project ProjectID

	// Number is the file's 64-bit numeric id within its share (§6.5): set at
	// create, never changed, never reused in the share. Never zero.
	Number uint64

	// CreateVerifier is the verifier of the exclusive create that made the
	// file (§2.10); nil when it was not made exclusively, and cleared by the
	// first SetAttrs.
	CreateVerifier []byte

	// Type-specific fields: each is set for its type only and empty otherwise.
	Target   []byte   // Type Symlink: the target, stored as given, never resolved here
	Device   DeviceID // Type BlockDevice, CharDevice: major and minor
	StreamOf FileID   // Type Stream: the file it belongs to (§2.7)
}

type FileType uint8

const (
	Regular FileType = iota + 1
	Directory
	Symlink
	BlockDevice
	CharDevice
	FIFO
	Socket
	Stream // a named stream (§2.7); never has an entry
)
```

**It is protocol-neutral.** Owners are principals ([RFC 16](rfc-16-metadata-store.md)), not uid/gid pairs.
A `Principal` is an **opaque ID minted by the store and never reissued**, as a
`FileID` is: a UID, GID or SID is a row of the store's protocol-ID index that
maps to a principal, never the principal itself, and the adapter resolves
through that index. So a file, an ACL entry or a quota never carries a number
another installation may have given to someone else, and an import maps
principals by ID and refuses one that collides ([RFC 12 §5.1](rfc-12-snapshots.md#5.1%20Layout)).

`Mode` stays as permission bits because both protocols expose it, but it is not the authority when an ACL
exists ([§2.6](#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)). SMB's DOS attributes are generic `Flags`, and `Birth` is SMB's
creation time and NFSv4's `time_create`. SMB symlinks and junctions map to
`Symlink`; other SMB reparse points are out of scope, and an adapter refuses to
create them.

**There is no generation.** A generation exists where file IDs are reused, to
tell a handle to a released file from one to its successor. A `FileID` is a
UUID that is never reissued, and every per-file key is scoped by its share
([RFC 16](rfc-16-metadata-store.md)), so a handle to a released file finds nothing and resolves stale
([§6.3](#6.3%20Staleness%20is%20reported%2C%20never%20guessed)). A restore makes a new share with a new `ShareID` and so new keys
([RFC 12 §3.3](rfc-12-snapshots.md#3.3%20Restore)), and cannot alias a handle to the original; nothing rolls a share
back in place ([RFC 12 §1.1](rfc-12-snapshots.md#1.1%20Non-goals)), so a released file's handle never resolves again. What clients do need is a
per-file change counter, and that is `Version`.

A file **MUST NOT** carry its own name, its own path, or a list of the entries
that name it. It is named *by* entries; it does not name itself. A name stored
on the File is a second copy of the entry, and rename then has to keep two
records in step for no gain.

`Parent` exists only for directories, and only because `..` has to resolve
without a search. It is exact, because a directory has exactly one entry naming
it ([§4.1](#4.1%20%60nlink%60%20is%20exactly%20its%20entries)).

Whether a file has an ACL is not a field: it is the store's own flag. A caller
asks for the ACL, and always gets one ([§2.6](#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)).

### 2.2 Entry

```go
// Entry is one name in one directory. A file with two hard links has two
// entries and one File.
type Entry struct {
	Parent FileID
	Name   []byte   // the bytes as given, validated at the boundary (§3.2); the key may be folded (§3.3)
	Child  FileID
	Type   FileType // copy of the child's type, written with the entry
}
```

One entry is one name in one directory. The child type is carried so that a
listing does not have to read every file it returns; it is a copy, and it
**MUST** be written in the same transaction as the entry, never refreshed later.

An entry points at its file (`Child`); a file does not point back at its
entries. The one exception is a directory's `Parent`. Nothing on a request path
needs the reverse: NFS works from handles, and an SMB open carries the path it
was opened by. No reverse index — the names of a file — is written or reserved
([§13](#13.%20Open%20questions)).

**An entry is its own record.** An implementation **MUST NOT** store a
directory's entries as one value, one document or one row holding the list. This
is [RFC 6 §2.1](rfc-6-block-metadata.md#2.1%20ChunkRef)'s rule with a different key, and it fails the same two ways: a
directory of *N* entries costs O(*N*²) to fill, and the list becomes a key that
every concurrent create, unlink and rename in that directory contends on.

**The entry is not merged into the file, and this was measured.** Creating a file
writes two records, a `File` and an `Entry`, in one transaction. Inlining the
file into its entry was benchmarked against it ([RFC 16](rfc-16-metadata-store.md), Appendix A):

- **Creates:** no difference within noise, serial or parallel. What limits a
  create is the parent directory's times ([§9.2](#9.2%20Timestamps)), not the number of keys.
- **Reads:** inlining is faster — `LOOKUP` 2.2×, a listing with attributes 5.5×.
- **What inlining cannot keep:** clients address files by handle, not by name,
  so an inlined layout still needs a key from `FileID` to its entry, written on
  every create and rewritten on every rename — the second record returns, and
  `GETATTR` by handle becomes two reads. A hard link forces the file out of the
  entry into its own record: a second code path for one file.

So the split stays, and the one real cost — a listing with attributes reading
each child — is paid down by reading the children in one batched pass
(`EntriesPlus`, [§3.4](#3.4%20Enumeration)), not by changing the records.

A directory is large because a user made it large. Nothing else in this system
lets one client's behaviour choose the cost of another's.

### 2.3 The name is not the identity

Every other component in the set keys content by `FileID` ([RFC 0 §3](rfc-0-data-lifecycle.md#3.%20Identity)). This is
the component that owns the mapping from a name to that identity, and it is the
only one allowed to hold it.

A path **MUST NOT** appear in a handle ([§6.1](#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)), in a lock ([RFC 14](rfc-14-open-state.md)), in a ref
([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)) or in a journal key ([RFC 1](rfc-1-journal.md)). Each of those outlives a rename, and a
path does not.

### 2.4 Attributes, and who writes them

| Attribute | Changed by | Written in the transaction of |
| --- | --- | --- |
| `Owner`, `Group` at create | the create: the caller's principal, and the credential's group, the principal's primary group or, with neither, the caller's own principal; under a setgid parent, the parent's group | the create |
| `Mode`, `Owner`, `Group`, `Flags` | chmod, chown, set-flags, ACL change; setuid and setgid also cleared by an unprivileged caller's write ([§9.6](#9.6%20setuid%20and%20setgid%20are%20cleared%20when%20an%20unprivileged%20caller%20changes%20the%20file)) | its own operation |
| `Access` | read, and only if the policy records it ([§9.2](#9.2%20Timestamps)) | its own operation |
| `Size`, `Charged`, `Applied`, `Modify`, and `Change` and `Version` on write, and an explicit set of size or `Modify` | a client write, truncate, deallocate, clone or copy into the file, a set-attribute | **existence** ([§2.5](#2.5%20Where%20%60size%60%20lives)) |
| `Change`, `Version` on attribute change | chmod, chown, set-flags, ACL and xattr change, link, unlink, rename: each draws a `Version` and stores it in the File in its own transaction ([§9.4](#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)) | its own operation |
| `Nlink` | link, unlink, rename over an existing entry | the entry change that caused it ([§4.1](#4.1%20%60nlink%60%20is%20exactly%20its%20entries)) |
| a directory's `Modify`, `Change`, `Version` | create, unlink, rename in it | the entry change, as a delta ([§9.2](#9.2%20Timestamps)) |
| `Number` | create only; never changed ([§6.5](#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)) | the create |
| `CreateVerifier` | set by an exclusive create; cleared by the first `SetAttrs` ([§2.10](#2.10%20Exclusive%20create)) | the create; the `SetAttrs` |

**A new file's owner and group come from the caller.** A create **MUST** set
`Owner` to the caller's principal. It **MUST** set `Group` to the parent
directory's `Group` when the parent has the setgid bit, and otherwise to the
credential's group: the group the request's credential names, where the
protocol's credential carries one (NFS's primary gid, resolved to its principal),
and the principal's primary group ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)) where it does not, as for SMB. A
principal with neither — no group on the credential and no primary group — gets
its own principal as `Group`, a group of one, so the file is shared with nobody
the owner did not choose; the create is not refused. A
directory created under a setgid parent is itself setgid, so the rule carries
down the tree. The rule is the same for every protocol, applied to what each
protocol's credential carries. A fixed default group **MUST NOT** be used: it
hands every file to whoever else holds that group, and makes the same user's file
differ by the protocol that created it. An owner or group the create request
itself names — NFSv4 create attributes, the owner or group of an SMB create's
security descriptor — is applied after this rule as a change of owner or group
in the create's own transaction, permitted or refused exactly as that change
would be on the new file; a create whose named owner or group is refused fails
whole, and is never made under another owner instead.

> decision: a creator with no group gets itself as the file's group rather than
> a refused create, because SMB principals without a primary group are common
> and refusing would make them unable to create anything. It grants nothing
> beyond what the owner already has. Refuse instead if an identity source is
> shown to report "no primary group" for principals that do have one, where the
> fallback would hide a mapping fault.

**A setgid bit is not inherited by a non-member.** When a non-directory is
created with the set-group-ID bit in its requested mode, and the caller is
neither a member of the `Group` the file gets nor privileged ([§7.4](#7.4%20The%20identity%20arrives%20resolved)), the create **MUST** clear the
bit, as Linux does since CVE-2018-13405. Under a setgid parent the file takes the
parent's group whatever the creator's memberships, so without this rule a user
outside `finance` creating a file with mode 2755 in a mode-3777 `finance`
directory makes a program that runs with `finance`'s rights. Likewise a `chmod`
that sets the set-group-ID bit on a non-directory **MUST** clear it when the
caller is neither a member of the file's `Group` nor privileged, as POSIX
`chmod` permits. A directory keeps the bit in both cases: on a directory it
names the group new entries take, and grants nothing to run.

The offload commit appears nowhere in that table, and **MUST NOT** ([RFC 6 §5.1](rfc-6-block-metadata.md#5.1%20No%20record%20is%20written%20by%20both%20paths)).
Offloading changes where content is, not what it is, and a File it could write
would be a record the client path and a background pass share, which
[RFC 6 §5](rfc-6-block-metadata.md#5.%20Write%20sets) forbids.

The table names where `Version` is stored, not when a client first sees it move.
A write advances the `Version` that `GETATTR` returns when the primary accepts
it, through the engine's overlay, before the existence commit stores it
([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr)). How `Version` is drawn, so that it never moves backward across
restarts and primaries, is [§9.4](#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward).

### 2.5 Where `size` lives

**`size` is a field of `File`, stored once, and read with the rest of it.**
`Files.Get` is one read of the file's record ([RFC 16](rfc-16-metadata-store.md)) and, for a directory, a scan of
its deltas not yet folded, bounded by the fold backlog ([§9.2](#9.2%20Timestamps)): nothing else is
joined and nothing is computed. This is the one statement of its cost; others
cite it. It is the write path's field — only an existence commit
sets `Size`, `Charged`, `Applied`, `Modify` and the `Change` a write causes, and
advances `Version` with them ([RFC 6 §3](rfc-6-block-metadata.md#3.%20Existence)) — and every other operation leaves
the write fields alone.

Until the journal's writes to a file are committed, the committed `Size` lags
them. `GETATTR` is therefore the filesystem service's join ([RFC 17](rfc-17-vfs.md)), made at the
file's primary: `Files.Get`, then, while the primary's engine holds uncommitted
writes to the file, the engine's overlay of their size, write times and
`Version` applied over it. When the engine holds none, which is the usual case,
the File record is the whole answer. This component never asks for the overlay:
it is a leaf ([§1](#1.%20Purpose)), and one join, in one place, is the only read path for
size.

**The join reads one state of the file.** The primary serialises a file's
existence commits ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)), and a commit moves operations out of the overlay
into the File record. A join that read the File before such a commit and the
overlay after it misses the committed writes in both, and reports a lower size
and `Version` than one it already returned, which breaks a client's
close-to-open check. The join **MUST** therefore read both under the file's
commit serialisation, as a sequence lock: read the file's commit sequence, which
is odd while a commit is applying; read the File; read the overlay; read the
sequence again; and retry when the two sequence reads differ or either is odd. A
join that keeps retrying past a bound takes the serialisation for one read.
Neither waits on a client.

**Every attribute a client sees is that join.** `Lookup`, `Create`, `SetAttrs`
and the listings return the File record this component holds; the filesystem
service applies the join before any of it reaches a client ([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr)). A
committed File returned while the primary holds uncommitted writes to it reports
a size and a `Version` older than a `GETATTR` already returned.

Three rules, in order of how much they cost to get wrong:

1. **`size` and the hole set move together.** A write past EOF grows `size` *and*
   adds a hole for the gap it skipped ([RFC 6 §3.5](rfc-6-block-metadata.md#3.5%20Operations%20that%20make%20holes)). Both **MUST** be written by
   the same existence commit. Written apart, the crash between them leaves a
   `size` covering an extent no hole records and no journal holds — content
   claimed to exist that was never written, which resolves **Lost** and fails a
   read that should have returned zeros.
2. **Only the write path writes `size`.** A `SetAttrs` that changes size is
   applied through an existence commit, never by writing the File directly. A
   `chmod` and a write to one file therefore both write its record, and conflict;
   the conflict is retried ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)). Keeping the two halves in separate
   records to avoid that rare conflict cost 25% on `GETATTR` and 60% on listings
   with attributes when measured, and is not done.
3. **There is one copy.** An implementation **MUST NOT** keep a second `size`
   anywhere — "for `GETATTR` speed", in a cache refreshed in the background, or
   on an entry. `GETATTR` and a read would answer from different records, and
   the disagreement is silent in both directions.

### 2.6 ACL, and how it agrees with the mode

```go
// ACL is the one access-control list a file has. Its semantics —
// evaluation, inheritance, and the NFSv4 and Windows mappings — are RFC 19's.
// This component stores it, keeps Mode in agreement with it, and evaluates it
// at the one chokepoint (§7.1).
type ACL struct {
	Entries []ACE
	Flags   ACLFlags // protected, auto-inherited, …
	Audit   []ACE    // audit and alarm entries: never grant or deny; they select audit events (§7.6)
}

type ACE struct {
	Type  ACEType // allow, deny, audit, alarm
	Flags ACEFlags
	Mask  AccessMask
	Who   Principal
}
```

**`Mode` and the ACL always agree, and neither is updated alone.**

- A file with no stored ACL is governed by `Mode`. Asked for its ACL, this
  component returns the one `Mode` implies (`ACLFromMode`), so there is always an
  ACL to evaluate and never a flag to check first.
- Setting an ACL writes the ACL and the `Mode` it implies in one transaction.
- `chmod` on a file with an ACL **merges**, exactly as [RFC 8881 §6.4.1.1](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4.1.1)
  specifies, and stores the ACL and `Mode` in one transaction:
  - the owner, group and everyone entries are rewritten to match the new mode;
  - every allow entry for a named principal is limited to the new mode's group
    bits, so `chmod 600` leaves no named user or group with access;
  - an inherit-only entry is left untouched: it governs children, not this file;
  - an entry that is both effective and inheritable is split into an
    inherit-only copy, unchanged, and an effective copy, limited as above;
  - deny, audit and alarm entries are left untouched.
- `Mode` lives on the File, so `GETATTR` never reads the ACL.

An implementation **MUST NOT** use "last writer wins" — whichever of `Mode` or
the ACL was set last being the authority, the other silently stale. NFSv4
requires the two to agree ([RFC 8881 §6.4](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4)), and a client that reads one and acts
on the other gets a permission it was never granted. A merge that rewrote only
the owner, group and everyone entries would fail the same way: `chmod 600`
would keep granting a named user access, and the ACL's implied mode would no
longer equal `Mode`.

### 2.7 Extended attributes and named streams

```go
// Xattr is one small named value on a file.
type Xattr struct {
	File  FileID
	Name  []byte
	Value []byte // bounded by a setting (RFC 13); larger data is a named stream
}
```

Each xattr is its own record, never a list on the File ([§2.2](#2.2%20Entry)'s rule, again).
A share setting bounds how many a file may have, as another bounds its streams,
so the release that deletes them stays one bounded transaction ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)).

A **named stream** (SMB's alternate data stream) has content, so it is a `File`
with its own `FileID`, `Type` `Stream` and `StreamOf` set. Its bytes go through
the journal and [RFC 6](rfc-6-block-metadata.md) like any file's; nothing in the content path learns that
it is a stream. Where identity shows, it follows the file it belongs to:

- it has no entry, and is reached only by listing its file's streams;
- it has no owner, group, mode or ACL of its own: every check on it is a check
  on its base file;
- the file id reported for it is its base file's;
- it has no entry and so `Nlink` zero, yet is never released by that: the base
  file's release writes its pending release, and it is then released as a file
  of its own ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)).

### 2.8 A share is one filesystem

**Its root is a File.** A share's root is a `File` of type `Directory` with no
entry: the share names it, and its `Parent` is itself, so `..` at the root
stays at the root. Each share reports its own filesystem identity — NFS's
`fsid`, SMB's volume serial — derived from its `ShareID`. SMB's several shares
are several tree connects, one share each. NFSv4's pseudo-filesystem, the
synthetic tree joining every export, is **not stored**: the filesystem service
builds it from the share list ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)), and a `LOOKUP` that crosses from it into a share lands on
that share's root. Shares are disjoint trees; one share is never an entry in
another, and no share's path is an ancestor of another's
([RFC 16 §2.3.1](rfc-16-metadata-store.md#2.3.1%20Share%20names%2C%20paths%20and%20state)).

```go
// FilesystemInfo is what a share reports about itself: statfs, FSSTAT,
// FSINFO and SMB volume information.
type FilesystemInfo struct {
	Share        ShareID
	Capabilities Capabilities // from configuration (RFC 13), fixed while mounted
	Usage        Usage        // counted (RFC 16)
	Quota        Quota        // the share-wide limit, if any
}

type Capabilities struct {
	CaseSensitive, CasePreserving bool
	MaxNameBytes                  int
	ACLs, Xattrs, Streams, Links  bool
	TimeGranularity               time.Duration
}

type Usage struct {
	Bytes, Files int64
}

// Quota is one limit: on the share, on a user or group in it, or on a tree.
// Limits are written by the management API; usage is counted. They are
// separate because they have different writers.
type Quota struct {
	Share     ShareID
	Principal Principal     // a user or group quota; zero otherwise
	Project   ProjectID     // a tree quota; zero otherwise
	Hard      Limit         // refused beyond it
	Soft      Limit         // allowed beyond it for Grace, then refused as Hard
	Grace     time.Duration // how long usage may stay above Soft
	Advisory  Limit         // never refused: crossing it only emits an event
}

// Limit is a byte and a file count; zero in either means no limit on it.
type Limit struct {
	Bytes, Files int64
}

// PrincipalUsage is one principal's usage in one share: what NFS rquota
// and SMB per-user quotas report and enforce.
type PrincipalUsage struct {
	Share     ShareID
	Principal Principal
	Usage
}
```

**What is charged is logical bytes, per file**: a file's size with its holes
excluded, held in `File.Charged` and charged to its `Owner`, its `Group` and its
`Project`. Each file is charged **in full**, whether its chunks are shared with
another file — by a clone, a copy or a deduplication hit — or not. Deduplication
and compression **MUST NOT** reduce a principal's usage; their savings are
reported for the share. A user's usage then depends neither on other users' data
nor on when GC runs. The existence commit that changes the size or the holes
changes `Charged` and the usage by the same delta, so truncating or punching a
hole lowers the charge without scanning holes.

`chown` and `chgrp` move exactly `Charged` from the old principal to the new one
in the transaction that changes the owner. A tree quota is carried by
`Project`: inherited from the parent at create, and a rename or link into a
different project **MUST** be refused (cross-device), so a tree's usage is one
counter and never a walk of ancestors. How usage is counted without a hot key
is [RFC 16](rfc-16-metadata-store.md)'s. Enforcement is the filesystem service's, at the file's primary
and at write time, against an in-memory reservation per principal and project
released when the write's existence commits ([RFC 17](rfc-17-vfs.md)): a write is checked
before the charge exists, so the overshoot is bounded by each primary's
reservation slack, and that bound is stated, not hidden.

**Soft and advisory limits.** Usage above `Soft` is allowed for `Grace`,
measured from when it first rose above `Soft`, a time recorded with the usage
([RFC 16](rfc-16-metadata-store.md)) and cleared when it falls back; after that, `Soft` is refused as
`Hard` is. `Advisory` is never refused. Crossing any limit, a grace that
expires, and every refusal emit a quota event through the event hook
([§7.6](#7.6%20Decisions%20are%20observable)), so an operator learns of a full tree before its users do.

### 2.9 Methods on File and Entry

Entities carry **pure** methods — no I/O, no context, no store — so that a rule
stated once here is written once in code:

```go
func (f File) IsDir() bool                      // and IsRegular, IsSymlink, IsStream, IsSpecial
func (f File) IsRoot() bool                     // Type Directory and Parent == ID
func (f File) HasContent() bool                 // Regular or Stream: goes through the content path
func (f File) HasFlag(x FileFlags) bool         // and IsHidden, IsReadOnly, IsImmutable, IsAppendOnly
func (f File) FSMode() fs.FileMode              // type bits plus Mode, for io/fs and logs
func (f File) Validate() error                  // type-specific fields set only for their type
func (f File) Apply(a Attrs) File               // the File a SetAttrs would produce

func (t FileType) String() string               // "regular", "directory", …

// Entry implements io/fs.DirEntry.
func (e Entry) Name() string
func (e Entry) IsDir() bool
func (e Entry) Type() fs.FileMode

func ACLFromMode(mode uint32, owner, group Principal) ACL // the ACL a file with no ACL has
func (a ACL) Mode() uint32                                // the Mode this ACL implies
func (a ACL) WithMode(mode uint32) ACL                    // chmod's merge (§2.6)

func (a Attrs) WithMode(m uint32) Attrs         // builders set the mask; and WithOwner, WithTimes, WithSize, …
func (u Usage) Exceeds(l Limit) bool            // and Add
```

Permission evaluation is deliberately not a method: it needs the principal's
groups and the share's grant, so it happens behind the one chokepoint ([§7.1](#7.1%20One%20chokepoint)).
No entity has `Save`, `Reload` or a pointer to the store.

### 2.10 Exclusive create

NFS's exclusive create (NFSv3 `EXCLUSIVE`, NFSv4 `EXCLUSIVE4` and
`EXCLUSIVE4_1`) carries an 8-byte verifier so that a retry of a create whose
reply was lost can be told from a second client's create of the same name. The
create stores the verifier in the File's `CreateVerifier`, in its own
transaction. An exclusive create that finds the name taken **MUST** succeed,
returning that file, when the file's `CreateVerifier` equals the request's,
and **MUST** fail with "exists" otherwise. The first `SetAttrs` that writes the
File record clears it, in its own transaction: a client follows an exclusive
create with that set-attribute, after which a retry is rightly an error.

The verifier **MUST NOT** be stored in a time attribute, as some servers do: the
time would read back as the verifier until the set-attribute, and a write's
`Modify` would erase the verifier before the client stopped retrying. A retried
create that crosses a failover relies on it, because the primary's dedup table
is lost ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)).

## 3. Names

### 3.1 Lookup resolves a name to a file, and that is all it does

    Lookup(parent, name) → file | none

A lookup **MUST** be O(log *n*) or better in the number of entries in the
directory, and **MUST NOT** be answered by enumerating it. A backend that
answers lookup with a scan turns every path resolution into a directory read,
so a deep path in a large tree costs the sum of every directory along it. A
lookup reads the entry, then the file it names: two reads, which is the price
of the split [§2.2](#2.2%20Entry) keeps for handles and hard links.

Resolution of a multi-component path is the caller's loop over this operation,
one directory at a time, with a permission check on each ([§7.2](#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always)). This component
**MUST NOT** offer a whole-path resolve that skips the intermediate checks.

### 3.2 A name is bytes, and it is validated at the boundary

A name is a byte string. This component **MUST** reject:

- the empty name;
- a name containing the path separator or a NUL;
- `.` and `..` as stored entries — they are resolved, never recorded ([§3.4](#3.4%20Enumeration));
- a name longer than the configured maximum.

It **MUST NOT** normalise, case-fold, transcode or otherwise rewrite a name it
was given: the name stored in the entry and returned by a listing is the one
that went in. A name that comes back different is a name the client cannot use,
and the client has no way to discover the rule that changed it. Folding the
*key* on a case-insensitive share ([§3.3](#3.3%20Case)) is not a rewrite of the name.

Validation happens here, not in the adapters, because there is more than one
adapter and a rule enforced in one of them is a rule the other does not have.

### 3.3 Case

Case sensitivity is a property of the share, decided once at configuration, and
**MUST NOT** vary between adapters reaching the same share. A case-insensitive
share **MUST** preserve the case it was given and compare without it.

**On a case-insensitive share the entry is keyed by the folded name**; the
entry's value holds the name as given. The fold rule is the share's, fixed when
the share is created and recorded in the store's format record
([RFC 16](rfc-16-metadata-store.md)), so every node and every restart folds the same way. A lookup
folds the name it is given and reads one key: there is no second index, a miss
is one read, and a create of `readme` beside `README` conflicts on the key
itself. A listing returns the stored names in the order of [§3.5](#3.5%20A%20cookie%20survives%20concurrent%20mutation). On a
case-sensitive share the key is the name. What the fold rule is, and which names
a share accepts, is [§3.8](#3.8%20Names%20every%20protocol%20of%20a%20share%20can%20use).

Two adapters disagreeing about case on one share is not a cosmetic difference.
Under SMB semantics `README` and `readme` are one entry and the second create
fails; under POSIX semantics they are two, and a client that made both then
sees one of them disappear the next time the other adapter writes.

### 3.4 Enumeration

    Entries(parent, cursor)     → entries, in order, resumable
    EntriesPlus(parent, cursor) → each entry with its File

A listing **MUST** be answered by a scan bounded by the entries returned —
O(log *n* + results) — never by reading the directory whole to return a page of
it. `EntriesPlus` serves listings that need attributes (NFS `READDIRPLUS`, SMB
directory queries): it **MUST** read the children's File records in batches, not
one read per name.

`.` and `..` are synthesised at the boundary that needs them, from the directory
itself and its `Parent` ([§2.1](#2.1%20File)). They **MUST NOT** be stored, because a stored
`..` is a second copy of the parent pointer that rename then has to move.

### 3.5 A cookie survives concurrent mutation

A listing cursor **MUST NOT** be a positional index. It **MUST** be derived from
the ordering key of the last entry returned, so that an insert or a delete
elsewhere in the directory does not shift what the next page contains.

**The cursor is a 64-bit cookie that resolves without an index.** NFS gives a
listing cursor 64 bits, and a name does not fit in them. Entries are therefore
**ordered by a digest of their key**, not by the key: a 63-bit keyed hash of the
entry's key, raised to 3 when it falls below 3 ([§3.3](#3.3%20Case)), under a hash key drawn when the share is created and
recorded with its fold rule, so every node and every restart orders alike and no
client can choose names that collide. An entry is stored under
`(parent, digest, key)` ([RFC 16](rfc-16-metadata-store.md)); a lookup computes the digest and still reads
one key. The cookie after an entry is its digest: never 0, 1 or 2, the values
NFS reserves, and with the top bit clear. Resuming reads the entries whose
digest is greater than the cookie's: no record of the cookie is kept, the entry it
came from may since have been deleted, and any node can resume it.

Entries whose digests are equal form a **collision chain**, ordered by key. A page
**MUST NOT** end inside a chain: it ends before the chain or includes all of it,
so the digest alone says where to resume. A chain longer than a page is returned
whole, as an oversized page. At 63 bits a chain of two is likely only past about
4·10⁹ entries in one directory.

> decision: a listing is in digest order, not name order. A name-ordered cookie
> needs a cursor wider than NFS allows, or an index from digest to key written by
> every create and rename; the digest order costs neither, and clients sort what
> they display. Add the index if a protocol or an application is shown to depend
> on the server's order.

With a positional cursor, deleting an entry before the cursor shifts every later
entry down one, and the client silently skips one; inserting shifts them up, and
the client sees one twice. Neither is an error anyone observes — the listing just
comes back wrong, in a directory that was being written while it was read.

An entry that is created or removed during a listing **MAY** be returned or
omitted. An entry present and untouched throughout **MUST** be returned exactly
once.

The order is fixed when the share is created, so a cookie verifier, where a
protocol has one, is a constant of the share. It **MUST NOT** change on a
mutation: a verifier that did would restart every listing of a busy directory
forever.

### 3.6 A structural change guards the directory it depends on

A directory's times change by delta ([§9.2](#9.2%20Timestamps)), so no create, link or rename
rewrites its parent's record, and the parent's record is no longer a write
every structural change in the directory collides on. What those changes still
depend on has to be made a conflict explicitly:

- **Create, link and rename-into** **MUST** guard the parent directory's File
  record ([RFC 16](rfc-16-metadata-store.md)'s `Guard`), in their own transaction. A guard is **shared**:
  guards do not conflict with each other, only with a write of the guarded
  record. Parallel creates in one directory therefore do not serialise.
- **Removing a directory** **MUST** write the directory's File record in the
  transaction that proves it empty. That write conflicts with every concurrent
  guard of it, so a create racing the removal either commits first — and the
  emptiness scan sees its entry, or the removal's write conflicts with its guard
  — or aborts.
- **The rename loop check** guards every ancestor it reads ([§5.2](#5.2%20The%20loop%20check%20is%20inside%20the%20transaction)).

A read inside the transaction is not enough. A range scan is not conflict-tracked
on every backend, and one backend validates no reads at all ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)). On
it, a removal that only scanned for entries commits beside a create that only
wrote one, and leaves an entry, and a file with `Nlink` 1, under a deleted
directory.

### 3.7 No short names

A file has the names its entries hold and no other. No 8.3 short name is
generated, stored or resolved: a lookup of one finds nothing, and SMB's
alternate-name query (`FileAlternateNameInformation`) is answered not
supported.

> decision: short names are not generated. Each would be a second entry per
> name, kept unique per directory and moved by every rename — a cost on every
> create and rename, for a need current Windows clients no longer have.
> Generate them, as a second entry kind, if an application in the target
> workload is shown to need one.

### 3.8 Names every protocol of a share can use

A share is reached over NFS and SMB, and a name one protocol can create the other
may be unable to name. The share's **name rule**, chosen at creation and recorded
with its fold rule ([§3.3](#3.3%20Case)), decides which names exist, and this component
enforces it for every protocol, at the boundary of [§3.2](#3.2%20A%20name%20is%20bytes%2C%20and%20it%20is%20validated%20at%20the%20boundary):

- **`posix`**, the default on a case-sensitive share: every name [§3.2](#3.2%20A%20name%20is%20bytes%2C%20and%20it%20is%20validated%20at%20the%20boundary)
  accepts. Over SMB, a character Windows forbids is shown through the SMB
  adapter's reversible mapping; a reserved device name is listed as stored, and a
  Windows client may be unable to open it.
- **`windows`**, the default on a case-insensitive share: additionally refuses a
  name that is not valid UTF-8; a name containing a character MS-FSCC §2.1.5.2
  forbids (`"`, `*`, `:`, `<`, `>`, `?`, `\`, `|` and the controls 0x01–0x1F);
  a name ending in a space or a dot; and a reserved device name — `CON`, `PRN`,
  `AUX`, `NUL`, `COM0`–`COM9`, `LPT0`–`LPT9`, in any case, alone or followed by a
  dot and anything, as `con.txt`. Over NFS such a create fails as an invalid
  name, so no client makes a name the share's Windows clients cannot open.

**Neither names nor keys are normalised.** A stored name is the bytes given
([§3.2](#3.2%20A%20name%20is%20bytes%2C%20and%20it%20is%20validated%20at%20the%20boundary)). On a case-insensitive share the fold rule is NTFS's: the name's
UTF-16 code units are each mapped through a 16-bit **upcase table**, one unit for
one, with no normalisation, and the table is recorded by ID with the rule
([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)). That is how NTFS and Samba compare names, and it is not
Unicode case folding: a supplementary-plane letter, whose units are surrogates,
is compared as given, and the Kelvin, Ohm and Angstrom signs and the capital
sharp s upcase to themselves rather than folding to `k`, `ω`, `å` and `ß`. So
`K.txt` and the same name spelled with the Kelvin sign U+212A are two entries,
as on NTFS; `Straße.docx` and `STRASSE.docx`, two files on a Windows volume,
stay two entries here; and a profile or project copied from Windows keeps every
file. On a case-sensitive share the key is the
bytes. On either, a name one client sends decomposed and another composed is two
entries, as on NTFS and on a local POSIX filesystem.

> decision: no normalisation in a stored name or in a key. Full case folding or
> NFC in the key would merge names Windows keeps apart (`ß` against `SS`) and
> lose a file on every copy from a Windows volume; the cost of leaving them out
> is that a decomposed and a composed spelling of one name are two entries,
> which users may take for one. Add a normalising fold rule, as a second
> recorded rule a share opts into ([RFC 16 §4.6](rfc-16-metadata-store.md#4.6%20Store%20format)), if clients of mixed normalisation
> are shown to create such pairs on a share.

## 4. What keeps a file alive

### 4.1 `nlink` is exactly its entries

A file's `Nlink` **MUST** equal the number of entries naming it, at every
commit point, and **MUST** change in the same transaction as the entry that
changes it.

This is [RFC 6 §6.1](rfc-6-block-metadata.md#6.1%20A%20refcount%20is%20exactly%20its%20refs)'s rule for refcounts, one level up, and it fails the same two
ways. Drifting high leaks a file and everything it references. Drifting low
releases a file a name still resolves to, so the entry survives pointing at
nothing.

A decrement that would take `Nlink` below zero **MUST** fail the transaction and
be reported as a consistency error naming the file. It **MUST NOT** be clamped:
the count was already too low before the decrement, so something else still
names the file ([RFC 6 §6.3](rfc-6-block-metadata.md#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary)).

Hard links to directories **MUST** be refused, which is what makes `Parent`
exact and what makes the rename loop check terminate ([§5.2](#5.2%20The%20loop%20check%20is%20inside%20the%20transaction)). A directory's
other than a share's root therefore has `Nlink` 1; the adapter reports whatever
its protocol expects.

**Two kinds of file have no entry and are never released by `Nlink`.** A share's
root ([§2.8](#2.8%20A%20share%20is%20one%20filesystem)) and a named stream ([§2.7](#2.7%20Extended%20attributes%20and%20named%20streams)) both have `Nlink` zero from the moment
they are created. Neither ever gets a pending-release record from an entry
change: a root goes only with its share, and a stream only through its base
file's release, which writes the stream's pending release ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)). The
release predicate below is therefore stated over files that have a pending
release, not over every file whose `Nlink` reads zero.

### 4.2 Open state is the second holder

A file with `Nlink` zero that is still open **MUST NOT** be released. Its
entries are gone and no name resolves to it; its content is still readable
through the handles that were opened before the unlink, and stays so until the
last of them closes.

Open state is therefore a holder of the file in exactly the sense `Nlink` is,
and the release condition is both:

> A file is released when it has a pending-release record ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)), `Nlink`
> is zero, **and** no open state references it.

Only the removal of a file's last entry, or its base file's release for a named
stream, writes that record, so a share root and a stream whose base still lives
are never released by this rule ([§4.1](#4.1%20%60nlink%60%20is%20exactly%20its%20entries)).

**Open state is recorded lazily**, and it is [RFC 14](rfc-14-open-state.md)'s: held in memory by the
file's primary ([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state)), and made a durable open record only when it keeps
content alive — when an unlink removes the last entry of an open file, or when
a file with no entry is opened ([RFC 14 §9.1](rfc-14-open-state.md#9.1%20An%20open%20keeps%20a%20file%20alive)). Opens and closes of linked files
write nothing. The durable open records are the only record of who holds an
unlinked file; this component keeps no second list ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)).

### 4.3 Release is what block metadata sees

Releasing a file drops its refs and decrements the chunks they name
([RFC 0 §7](rfc-0-data-lifecycle.md#7.%20Mutation%20and%20removal), [RFC 6 §6.4](rfc-6-block-metadata.md#6.4%20Delete)), drops the journal's copy of its content, and deletes
its File, ACL and xattrs, and writes the pending release of each of its named
streams ([§2.7](#2.7%20Extended%20attributes%20and%20named%20streams)). It is the engine's `Release`
([RFC 8 §12.1](rfc-8-engine.md#12.1%20One%20content%20facade%2C%20called%20by%20the%20filesystem%20service)), called by the filesystem service at the file's primary; this component
never calls it ([§1](#1.%20Purpose)) and never drops a ref itself. A release that drops the
refs and leaves the journal holding the file is half a release. Block metadata is
never told the name that was removed ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)).

**The removal of a file's last entry always writes a pending-release record**,
in the transaction that removes the entry — an unlink, or a rename over the
file's last name — whether or not the file is open. The record names the file
and holds nothing else: no holder list and no lease. If the file is open, the
same transaction makes its opens durable ([RFC 14 §9.1](rfc-14-open-state.md#9.1%20An%20open%20keeps%20a%20file%20alive)).

When the entry's directory and the file are in different shards, the transaction
runs at the directory's primary, which cannot see the file's opens: the file's
primary makes them durable when it prepares the unlink, before the transaction
commits, and keeps doing so for opens it admits until the outcome; the transaction
writes the pending release all the same and leaves the decision to the file's
primary ([RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries)).

**Only the release transaction deletes it.** The filesystem service at the file's
primary runs the release ([RFC 17 §5.3](rfc-17-vfs.md#5.3%20Open%20and%20close)) once no open references the file: at once
when none does, when open state reports the last close ([RFC 14 §9.1](rfc-14-open-state.md#9.1%20An%20open%20keeps%20a%20file%20alive)), or when
grace ends with no reclaimed open ([RFC 14 §9.2](rfc-14-open-state.md#9.2%20A%20new%20primary%20releases%20nothing%20before%20grace%20ends)). That transaction is the first transaction of the engine's `Release`
([RFC 8 §8.1](rfc-8-engine.md#8.1%20A%20removal%20is%20one%20transaction%2C%20then%20batches)): it records the removal, deletes the namespace records and
deletes the pending release `F‖id‖rel` ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)); the removal then masks
the refs, and its batches drop them. A crash
anywhere between the unlink and that transaction leaves the record, and the
primary's recovery releases every recorded file that no open holds. An unlink
acknowledged with nothing recorded, and then forgotten, would leak every chunk
of the file with nothing left to find them by — which is why the record is
written even when the release follows at once.

**The engine reaches that transaction only through `Existence.Remove`** with a
release removal ([RFC 6 §10.1](rfc-6-block-metadata.md#10.1%20Interface)), the one view it holds over these records, so that
call carries this component's half of the release. It **MUST**, in its one
transaction: run the holder re-check of [§4.5](#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction) and abort when it fails; delete
the File record, its ACL, every xattr, its delete-pending record and its
pending release; and write the pending release of each of its streams. The
xattrs are a prefix of the file's own keys ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), and a share setting
bounds both them and the streams, so the transaction stays bounded. A
`Remove` that dropped the content records and left any of these would leave a
File no handle can reach and no release will find.

A pending-release record is not a holder: it keeps nothing alive, it only
remembers a release that has not yet run.

**Streams are released with their file, each as a file.** A named stream has its
own `FileID` and its own content ([§2.7](#2.7%20Extended%20attributes%20and%20named%20streams)), so dropping the base file's refs
leaves the stream's. An open of a stream counts as an open of its base for
[§4.2](#4.2%20Open%20state%20is%20the%20second%20holder), so a base with an open stream is not released. The base's release
transaction **MUST** write a pending-release record for each of its streams, and
each stream is then released as a file of its own, by the same engine `Release`
the filesystem service runs for any pending release. A share setting bounds the
streams a file may have, so that transaction stays bounded.

### 4.4 There is no third holder

`Nlink` and open state are the only things that keep a file alive. An
implementation **MUST NOT** add a second mechanism — a hold list, a pin set, a
protected-file table, an extra root consulted by a sweep. [RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref) (M12)
gives the reason: a second mechanism fails open. A snapshot's content is held by
counted history refs ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), and an open-but-unlinked file is an ordinary
file with zero entries; both are already alive by the rules above.

### 4.5 A release re-checks its holders inside its own transaction

Deciding that no open holds a file, and then releasing it, are two steps, and an
open can arrive between them: a file with no entry can still be opened by
handle, and that open is recorded as a durable open ([RFC 14 §9.1](rfc-14-open-state.md#9.1%20An%20open%20keeps%20a%20file%20alive)). A release that
acted on the earlier observation would delete a file an open now holds.

- The release transaction **MUST** verify, inside itself, that the file's
  `Nlink` is zero, that its pending-release record exists, and that no durable
  open record names the file or one of its streams, and **MUST** abort, releasing
  nothing, if any check fails. It deletes the File record, so it conflicts with
  every transaction that guards that record.
- An open of a file that has lost its last entry — `Nlink` zero, not a share
  root; for a named stream, an open whose base file has lost its last entry,
  guarding the base's File record — **MUST** guard the File record
  ([§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on)) in the transaction that writes its durable open record, and **MUST**
  fail as stale when the File is gone. Either the open commits first and the
  release sees its record, or the release's delete conflicts with the open's guard
  and one of them retries.
- `Link` **MUST** refuse, as stale, a target whose `Nlink` is zero: a released
  file cannot be given a name back through a handle, and a link racing the release
  would name a deleted file.

The guard is needed, not only the check, for the reason [§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on) gives: a scan
of open records is not conflict-tracked on every backend.

## 5. Rename

### 5.1 One transaction

Rename removes the source entry, installs the destination entry, and adjusts
every count those two changes imply — in **one transaction**:

- the source entry is removed;
- the destination entry is installed, replacing an existing entry if one is
  there;
- the replaced entry's file has `Nlink` decremented, and, if that was its last
  entry, its pending release is written ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees));
- a renamed directory's `Parent` is updated;
- both directories' `Modify`, `Change` and `Version` advance (as deltas, [§9.2](#9.2%20Timestamps)),
  and the renamed file's `Change` and `Version` advance;
- the destination directory is guarded ([§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on));
- a rename into a different `Project` is refused ([§2.8](#2.8%20A%20share%20is%20one%20filesystem));
- an existing destination is replaced only as [§5.4](#5.4%20Rename%20over%20an%20existing%20entry) allows.

Partial application **MUST NOT** be observable. A rename visible at neither name
loses a file that was never deleted; a rename visible at both makes one file
reachable by two paths with `Nlink` one, and the first unlink of either then
releases content the other still names.

### 5.2 The loop check is inside the transaction

Renaming a directory into its own descendant **MUST** be refused, and the check
**MUST** be evaluated in the same transaction that performs the rename. The
check walks the destination's ancestors through their `Parent`, and **MUST**
guard every ancestor it reads ([§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on)).

Guarding is what makes the check hold concurrently. Renaming A under B writes
A's record and reads B's ancestors; renaming B under A writes B's and reads A's.
The write sets are disjoint, and on a backend that validates no reads both
would commit and leave a cycle. With the ancestors guarded, each rename's write
conflicts with the other's guard, and one of them retries and is refused.

A check made before the transaction is vacuous: it walks the ancestors of the
destination, finds the source absent, and by the time the rename applies a
concurrent rename has moved the destination under the source. The result is a
cycle of directories that no path reaches, that `Nlink` says are alive, and that
nothing will ever release.

This is the same failure as [RFC 6 §7.1](rfc-6-block-metadata.md#7.1%20Conditional%20retirement)'s blind delete — a condition read before
the operation that changes it — and it needs the same remedy, not a lock taken
around the read.

### 5.3 Rename moves an entry and nothing else

A rename **MUST NOT** change a handle ([§6.1](#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)), invalidate a lock ([RFC 14](rfc-14-open-state.md)), touch a
ref ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), or move a byte. Everything below this component keys on the
file, which did not change.

An implementation that has to do work proportional to a file's size or its lock
count on rename has put the name somewhere it does not belong, and [§2.3](#2.3%20The%20name%20is%20not%20the%20identity) names
where to look.

### 5.4 Rename over an existing entry

Rename replaces a destination entry only when the two agree in kind, and checks
it in its own transaction:

- a directory **MAY** replace only an empty directory, and a non-directory only a
  non-directory; any other pairing fails, as "is a directory" or "not a
  directory";
- replacing a directory **MUST** prove it empty and write its File record in the
  rename's transaction, exactly as removing it does ([§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on)): a create racing
  into it either commits first, and the rename fails as not empty, or conflicts
  with that write and retries. The replaced directory's `Nlink` falls to zero and
  its pending release is written ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees));
- a rename whose source and destination are entries of one file **MUST** succeed
  and change nothing, as POSIX requires.

Without the write, a create that only guarded the replaced directory commits
beside a rename that only scanned it, and leaves a file with `Nlink` 1 under a
released directory.

### 5.5 A recycle bin is a rename, and keeps three constraints

A recycle bin turns an unlink into a rename into a bin directory, stamping a
deletion time, an original path and a deleting user. Nothing below the namespace
has to know a file is in one. Where a bin is offered, wherever it is built
([§13](#13.%20Open%20questions)), three constraints hold:

- a bin directory **MUST NOT** be owned by whichever user deletes into it first,
  so one user's deletion never decides who may read another's;
- a move into the bin that fails **MUST** surface the underlying refusal — an
  access error, not an I/O error — so the client keeps the file and says why;
- an option restricting the bin to administrators **MUST** be enforced by this
  component's permission check ([§7.1](#7.1%20One%20chokepoint)), or not offered.

## 6. Handles

### 6.1 A handle names a file, never a path

A handle is opaque ([RFC 0](rfc-0-data-lifecycle.md)'s rule for the set, and this component's to keep). It
is generated here and resolved here. No adapter parses one, constructs one, or
derives one from another.

A handle **MUST** encode:

- the **share** it belongs to, so the runtime can route without interpreting the
  rest;
- the **file**, by `FileID`.

**A `FileID` MUST be unguessable**: minted from a random source, never derived
from a counter, a clock or its parent's ID. A handle is a bearer token for an
NFSv3 client, and a guessable `FileID` lets a client that learned one handle
reach its neighbours without ever looking a name up. Minting IDs near their
parent's, to keep a create's records in one key range, is not adopted unless handles
are authenticated by a MAC the store keeps.

Nothing else is needed to refuse a handle to a released file: a `FileID` is
never reissued, and every per-file record is scoped by its share, so a released
file's handle finds no file and resolves stale ([§6.3](#6.3%20Staleness%20is%20reported%2C%20never%20guessed)). A restore into a new
share gets new keys under a new `ShareID`, so it can never answer a handle to the
original ([§2.1](#2.1%20File)).

A handle **MUST NOT** encode a path, a name, a parent, or an offset into a
directory. All four change while the file does not, so a handle carrying one is
a handle that breaks on an operation that was supposed to be invisible to it.

**A handle carries no shard, not even as a hint.** A file's shard changes when it
moves between shards ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)), and a handle carrying it
would change its bytes when a lookup after the move minted it again: one file,
two handles, which the one-spelling rule below forbids and which a client
comparing handles byte for byte takes for two files. The filesystem service
routes a handle by the file's `Shard` ([§2.1](#2.1%20File)), read through its route cache
([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20primaries)).

> decision: no shard hint in the handle, at the cost of one routing read per
> uncached file on a node that is not its primary. Carry a hint only in a part of
> the handle clients never see change, if that read shows in a split deployment's
> profile.

**A `FileID` is never on a wire outside a handle.** Every other identifier a
protocol reports — a numeric file id, SMB's 128-bit file id, an object id — is
derived as [§6.5](#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused) states. A `FileID` reported in clear hands anyone who can
query a file's information the secret half of its handle.

**Pseudo-filesystem handles are the one other kind.** A directory of the NFSv4
pseudo-filesystem ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)) is not a `File`: it has no `FileID`, no
share and no existence apart from its path, so its path *is* its identity and
the rule above does not reach it. Its handle is a distinct kind carrying a
digest of the installation's identity ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)) and the path, minted and resolved here
like any other; it resolves by finding a share path under that path, and is
stale when none remains. No handle of this kind names a file, so routing never
looks for a share in it.

**A handle has one spelling.** Decoding a handle **MUST** accept exactly one
byte form for each file, or canonicalise before the handle is used, and handles
**MUST** be compared by what they decode to. A parser that accepts several
spellings of one identity turns byte comparison into a lie: two handles for one
directory compare unequal, a lock keyed on the handle stops serialising, and a
rename between them takes the wrong path.

![A handle resolving straight to a file across a rename, beside a path-derived handle that the same rename breaks](img/rfc5-handle-identity.svg)

### 6.2 A handle is stable across restart

A handle **MUST** remain valid across a server restart for any file that still
exists. It follows from `FileID` and `ShareID` being durable ([RFC 0 §3](rfc-0-data-lifecycle.md#3.%20Identity)).

A handle derived from anything a restart re-derives — a table index, a pointer,
a hash of in-memory state — is a handle that every client has to rediscover
after a restart it was not told about.

### 6.3 Staleness is reported, never guessed

Resolving a handle returns one of three answers, and they are distinct:

| Answer | When | Reported as |
| --- | --- | --- |
| the file | it exists in the handle's share | success |
| **stale** | no file with that `FileID` exists in that share: it was released | stale-handle error |
| **not this share** | the handle's share is not the one asked | access error |

A handle whose file is gone **MUST NOT** resolve to another file, and **MUST
NOT** be reported as a missing file. "Not found" tells a client to create;
"stale" tells it to look the name up again. Answering the first for the second
makes a client recreate a file that a rename had merely moved.

### 6.4 Resolution does not touch the namespace

Handle resolution **MUST** be a direct lookup of the file, O(1) or O(log *n*),
and **MUST NOT** walk directories, resolve a path, or consult an entry record.

Every operation on an open file resolves a handle first. A resolution that walks
the namespace makes the cost of reading a file depend on how deep it was put and
how large the directories above it are, for the whole time it is open.

### 6.5 A protocol's numeric file id is a stored number, never reused

Some protocols report a fixed-width integer identifying a file, narrower than
the handle. It is never derived from the handle or the `FileID`: a truncated
hash of either is a silent aliasing of two files. Clients that treat the id as
identity — hard-link detection, `find -samefile`, backup tools deciding two
paths are one file — then conclude that two unrelated files are one, and back
up or restore only one of them.

**It is a stored number.** Every file carries `Number` ([§2.1](#2.1%20File)), a 64-bit
integer allocated at create from the share's allocator ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), which
each shard's primary reserves from in ranges so that creates do not contend on
it. A number is **never reused** within its share: a range a failed primary
had not used up is abandoned, not handed out again. Being a field, it is the
same across restart, failover and a move between shards, and a restore into a
new share may keep it, since it is unique only within a share. A restore that
keeps numbers **MUST** raise the destination share's allocator above the highest
`Number` it imports, in the import's transaction and before any create in the
share; otherwise the next create is issued a number a restored file holds, and
two files report one id.

- NFS's `fileid` is `Number`.
- SMB's 64-bit file id is `Number`; its 128-bit file id is a 64-bit keyed digest
  of the `ShareID` under an installation secret, followed by `Number`, so it is
  unique across the installation and reveals neither the `ShareID` nor a
  `FileID`.
- A named stream reports its base file's id ([§2.7](#2.7%20Extended%20attributes%20and%20named%20streams)).
- A file seen through a snapshot reports ids that differ from the live file's
  and from every other snapshot's ([RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot)). The allocator issues numbers
  below 2⁴⁸. A snapshot's 64-bit id is the top bit set, then 15 bits of the
  snapshot's **ordinal**, then the 48-bit `Number`; the ordinal is the smallest
  value below 2¹⁵ that no other live snapshot of the share holds, assigned with
  the cut, so a share holds at most 2¹⁵ snapshots at once. Each snapshot of a file
  therefore reports its own id, and over SMB each snapshot also reports its own
  volume serial, derived from the share and the snapshot's ordinal, never the
  share's. The high half of its 128-bit id is a keyed digest of the
  `ShareID` and the cut under the same secret.

`Number` is not an index: nothing resolves a number to a file.

> decision: a snapshot ordinal is reused once its snapshot is deleted, so a client
> that cached an id from a deleted snapshot can meet it again in a later one, as an
> NFS client meets a reused inode number. Never reusing ordinals needs more bits
> than the id has beside a 48-bit `Number`, and 2⁴⁸ numbers is 2.8·10¹⁴ creates
> over a share's life. Revisit if a share is shown to approach either bound.

> decision: opening by file id (SMB's `FILE_OPEN_BY_FILE_ID`) is refused as not
> supported. It needs an index from number to `FileID`, one more record per
> create, and it turns a guessable number into a way to reach a file without
> traversing to it ([§6.1](#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)). Add the index, checked against the traversal rules,
> if a workload's clients are shown to open by id.

## 7. Permissions

### 7.1 One chokepoint

Every permission decision is **made** in this component. A protocol handler
**MUST NOT** evaluate a mode, an ACL or a share's read-only flag itself, and
**MUST NOT** be able to reach an operation that was never authorised.

There is more than one adapter. A check that lives in an adapter is a check the
other adapter does not have, and the only way anyone discovers that is a client
reaching data through the protocol nobody audited.

### 7.2 Two timings, both allowed; one owner, always

Protocols differ on *when* they check, and this component accommodates both:

| Timing | The check | Later operations |
| --- | --- | --- |
| **per operation** | runs on every call, against the file | each is checked |
| **at open** | runs once, and the granted access is the result | gated on the grant |

Neither is wrong. An open-time grant is how a handle-oriented protocol is
specified, and re-deriving it per operation would change the semantics the
client was promised — an access revoked after the open would start failing
writes that the protocol says must keep succeeding.

What **MUST NOT** differ is the owner. A grant computed at open is a decision,
so this component computes it; it is stored with the open it belongs to
([RFC 14](rfc-14-open-state.md)) and evaluated here on the operations it gates. An adapter that
computes its own grant and passes the verdict back in as a flag has moved the
decision out of the one place [§7.1](#7.1%20One%20chokepoint) requires it to be, and the other adapter
cannot see the grant at all.

A grant **MUST** name the file it was computed against, and an operation
**MUST NOT** be gated on a grant computed against a different one.

### 7.3 The decision is against the file, not the name

A check **MUST** read the file the operation will act on, in the state the
operation will act on it. Checking a name, then acting on whatever that name
resolves to later, is two observations of a thing that can change between them.

For a path of several components, each directory is checked for traversal as it
is resolved ([§3.1](#3.1%20Lookup%20resolves%20a%20name%20to%20a%20file%2C%20and%20that%20is%20all%20it%20does)). The checks are not collapsible into one check on the leaf:
a caller with no right to enter a directory has no right to what is inside it,
however the leaf's own mode reads.

### 7.4 The identity arrives resolved

This component receives a resolved identity and applies it. It **MUST NOT** know
about export policy, squashing, authentication flavours or netgroups. Those are
applied by the filesystem service on every call, before this one
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)), and the identity that arrives here is
what the caller already is on this share.

**The share grant is not export policy.** A `ShareGrant` ([RFC 16](rfc-16-metadata-store.md)) is a record
of this store naming a principal's access to a share, and `Authorize` **MUST**
evaluate it on every call, not only at mount or tree connect, before the file's
mode or ACL. A client removed from a grant is then refused on its next call,
whatever handles it has cached. `Root` takes the identity, and is authorised
against the grant like every other call; the filesystem service calls it when
a client mounts or tree-connects ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)).

An identity **MUST** carry every field any check reads — including the ones a
particular backend's checks do not read — because the component that builds it
cannot know which check runs.

**Privilege is a field of the identity.** A *privileged* caller is one the
filesystem service marks so: an NFS caller whose root identity the export left
unsquashed, or a principal the share grant names `admin` — POSIX's
`CAP_FSETID` and `CAP_LINUX_IMMUTABLE`, as one flag. This component reads the
flag and never derives it from a UID or a group name. It decides three things:
whether setgid survives a create or `chmod` by a non-member ([§2.4](#2.4%20Attributes%2C%20and%20who%20writes%20them)), whether a
write clears setuid and setgid ([§9.6](#9.6%20setuid%20and%20setgid%20are%20cleared%20when%20an%20unprivileged%20caller%20changes%20the%20file)), and who may set or clear the immutable and
append-only flags ([§7.8](#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint)). The authorisation cache key carries it ([§7.5](#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read)).

### 7.5 A cached decision is keyed by everything it read

An implementation **MAY** cache a resolved identity or an authorisation result.
The key **MUST** include every field the cached value was derived from.

This is the failure that has to be named rather than implied. A key written
against the fields an identity had at the time it was written stays compiling,
stays passing, and silently starts colliding the moment the identity grows a
field — two callers whose old fields match are served one another's decision.
It fails **open**, and it is invisible to a search for the new field's name,
because the defect is in the key and the key never mentions it.

Adding a field to an identity is therefore an edit to every key derived from
one, and a key that cannot be shown to include the whole identity **MUST** be
replaced by one that does. A cached authorisation result is derived from the
share grant too, so its key **MUST** include the grant's version: a changed or
removed grant is a different key.

### 7.6 Decisions are observable

The chokepoint **MUST** emit an access-audit event for every decision an audit
or alarm entry of the file's ACL selects ([§2.6](#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)), and for every decision a
share-level audit setting selects: principal, file, access asked, outcome.
Quota events ([§2.8](#2.8%20A%20share%20is%20one%20filesystem)) go through the same hook. The hook is a
declared sink the filesystem service supplies ([RFC 17](rfc-17-vfs.md)); this component
emits to it and never waits on it, so a slow consumer drops events, counted,
rather than delaying a decision. The event stream's format and delivery — to a
log, an antivirus scanner or a ransomware detector — are a planned RFC's
([index](rfc-index.md)).

Because every decision is made here ([§7.1](#7.1%20One%20chokepoint)), one hook sees every access through
every protocol; an audit emitted by an adapter would miss the others.

### 7.7 What a caller may be asked for, and what removing a name needs

`Access`, what `Authorize` is asked for, is a mask of the rights both protocols
grant, not of mode bits: read data, write data, append, execute or traverse, read
and write attributes, read and write the ACL, change the owner, **delete**, and,
on a directory, **add a file**, **add a subdirectory** and **delete a child**. A
`Mode` grants each right its bits imply; an ACL grants what its entries say.

Removing a name is a decision about two files, and no single `Authorize` call
can make it. `Unlink`, `Rename` and `Link` therefore authorise inside their own
transactions, against the files they act on ([§7.3](#7.3%20The%20decision%20is%20against%20the%20file%2C%20not%20the%20name)):

- removing an entry is allowed when the caller has **delete** on the file or
  **delete a child** on its directory, as [RFC 8881 §6.2.1.3.2](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.2.1.3.2) specifies; in a
  directory with the sticky bit, governed by its mode alone, only the owner of
  the file or of the directory may remove it;
- a rename needs the removal right on the source entry; **add a file**, or **add
  a subdirectory** for a directory, on the destination directory; the removal
  right on an entry it replaces; and, for a directory moved to another parent,
  write on that directory, whose `..` changes;
- a link needs **add a file** on the directory.

An adapter never pre-checks any of these with `Authorize` and passes the verdict
in ([§7.2](#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always)).

### 7.8 Immutable and append-only flags are enforced at the chokepoint

`Flags` holds immutable and append-only ([§2.1](#2.1%20File)), and a flag stored but enforced
nowhere is a promise to the client that nothing keeps. This component enforces
both, in `Authorize` and inside the transactions of [§7.7](#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs), for every caller,
privileged or not, and for every protocol, as Linux does for
`FS_IMMUTABLE_FL` and `FS_APPEND_FL`:

| Operation | Immutable file | Append-only file |
| --- | --- | --- |
| write at an offset below the size, truncate, deallocate, clone or copy into it | refused | refused |
| write that only appends | refused | allowed |
| unlink, rename of it or over it, link to it | refused | refused |
| attribute, ACL or xattr change other than the two flags | refused | refused, except `Access` and `Modify` set by the write path |
| create, link or rename into it (a directory) | refused | allowed |
| unlink or rename out of it (a directory) | refused | refused |

Whether a write only appends depends on its offset and the file's size at that
moment, which `Authorize` alone does not see. The check therefore runs where the
primary accepts the write, inside the hold of the file's accept lock
([§9.2](#9.2%20Timestamps)), against the request's offset and the size the File
overlay reports there; an offset below that size is refused. Checked anywhere
earlier, another client's append accepted between the check and the write
would leave this write's offset below the size, and the "append" would
overwrite what that client wrote.

A refusal is a permission error (`EPERM`, `NFS4ERR_PERM`,
`STATUS_ACCESS_DENIED`), never an I/O error. Only a privileged caller ([§7.4](#7.4%20The%20identity%20arrives%20resolved))
**MAY** set or clear either flag, and clearing it is the one change an immutable
file accepts. An open granted before a flag was set does not outlive it: the
grant is evaluated against the file's current `Flags` on every write it gates
([§7.2](#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always)). SMB's read-only attribute is a different flag, which this table does
not cover.

## 8. Open state, as the namespace sees it

Opens, byte-range locks, deny modes, caching grants (delegations, oplocks,
leases), watches, client leases, grace and reclaim — including how locks are
keyed and that they never pin bytes — are specified in [RFC 14](rfc-14-open-state.md), and only
there.

### 8.1 An open keeps a file alive

Open state is the second holder of a file ([§4.2](#4.2%20Open%20state%20is%20the%20second%20holder), N2). This component does not
ask open state anything: it writes the pending release when a file loses its last
entry ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)), and the filesystem service consults open state before running
the release ([RFC 14 §9](rfc-14-open-state.md#9.%20Open%20state%20and%20the%20life%20of%20a%20file)).

### 8.2 A delete on close is an ordinary unlink, later

SMB's delete on close and delete pending are open state
([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)): they refuse new opens while the name stays, and are not
a third holder ([§4.4](#4.4%20There%20is%20no%20third%20holder)). When the last open closes, the filesystem service
unlinks the recorded entry through `Unlink`, as a client would, with two
differences: it runs as the principal of the open that set the delete mark, and
it is authorised by that open's grant ([§7.2](#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always)), not checked afresh, since the
right to delete was decided when the mark was set. The open record stores that
principal ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)), so the unlink has an identity even when no client call
causes it — a close by lease expiry, by revocation or at the end of grace. Run as
whoever closed last, the unlink would let any principal holding an open of the
file decide whether another's deletion happens; run with no identity, it has
nothing to authorise. This component sees nothing else of it.

**A delete-pending directory takes no new entries.** While a directory is delete
pending, the filesystem service, which holds the open state, **MUST** refuse a
create, a link or a rename whose destination parent is that directory, as it
refuses new opens of it: `STATUS_DELETE_PENDING` over SMB, an access error over
NFS. Otherwise a client fills a directory the deleting client was told is going,
and the close-time unlink fails as not empty. A create that passed the check
before the mark was set and commits after it is caught by the removal's own
emptiness proof ([§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on)): the unlink fails as not empty, the directory stays, and
the failure is reported to the closing open, never as a silent leak.

## 9. Attributes and what is not one

### 9.1 Attributes are answers, not caches

`GETATTR` reads the File ([§2.5](#2.5%20Where%20%60size%60%20lives)), and the filesystem service applies the
engine's write overlay over it while writes are uncommitted. An implementation **MUST NOT** hold a materialised attribute row
that a background pass refreshes: it is a cache that can outlive its inputs, and
[RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function) forbids exactly that shape for residency for exactly this reason.

### 9.2 Timestamps

`Modify`, and `Change` on write, are written with existence ([§2.5](#2.5%20Where%20%60size%60%20lives)). `Change`
and `Version` advance on every attribute and link change, `Modify` only on
content change. An implementation **MUST NOT** advance `Modify` for an operation
that changed no content — an offload, an eviction, a fill and a relocation all
leave it untouched, because none of them changed what the file is.

**A directory's times change by delta.** Every create, unlink and rename in a
directory advances its `Modify`, `Change` and `Version`. Read and rewritten in
every such transaction, the directory's record is the key every parallel create
in it contends on: measured, parallel creates in one directory fell from 111k/s
to 28k/s whatever the file layout ([RFC 16](rfc-16-metadata-store.md), Appendix A). The change is
therefore written as a delta record under the directory, in the entry change's
own transaction, without reading or rewriting the directory's record; the
transaction guards it instead, and guards do not conflict with each other
([§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on)). The store folds deltas into the directory, and reading the directory
applies any not yet folded ([RFC 16](rfc-16-metadata-store.md)).

**A fold does not race the creates it folds.** A fold rewrites the directory's
record, and that record is what every create, link and rename-into guards, so a
fold committing beside one would abort it. The directory's primary, where every
structural change of the directory runs ([RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries)), therefore **MUST** serialise a
fold against them in memory: it starts a fold only when no such change of that
directory is in flight, and admits none until the fold has committed. Only a
removal of the directory, which writes the record itself, still conflicts with
the guards.

> decision: the fold pauses the directory's creates for one transaction; folds
> are taken by backlog, not per create, so the pause is rare. Move the folded
> times to a record of their own that nothing guards — at the cost of a second
> read in every directory `GETATTR` — if that pause shows in a create benchmark. The change is never coalesced out of its
transaction. Each delta carries the `Version` drawn for it ([§9.4](#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)), and
folding takes the maximum. Reading a directory's times therefore reads its record
and the deltas not yet folded, a scan bounded by the fold backlog: the one-read
`GETATTR` of [§2.5](#2.5%20Where%20%60size%60%20lives) holds for a directory only when no delta is waiting.

**A directory's change info is not atomic.** NFSv4 reports, for every create,
remove, rename, link and creating open, the directory's change attribute before
and after the operation and whether the two bracket that operation alone. The
transaction that writes the delta never reads the directory's `Version`, and a
concurrent create in the same directory commits its own delta beside it, so no
exact pair exists to report. `before` and `after` **MUST** be the directory's
`Version` read just before and just after the transaction, and `atomic`
**MUST** be false. A client given `atomic` false revalidates the directory
rather than patching its cache, which is correct under any interleaving; a
pair reported atomic that another create fell between would leave that entry
out of the client's cache.

> decision: change info is reported non-atomic, always. Exact values need the
> directory's version at commit, which only reading the directory record — the
> contention the delta removed — or a version the primary assigns could give.
> Have the directory's primary assign each delta's version in memory, in its
> serialised order, and report `atomic` from it, if NFS clients' directory
> revalidation shows up in a profile.

**NFSv3 pre-operation attributes are omitted unless captured atomically.** NFSv3
returns an object's attributes before and after each change, and a client whose
cache matches the `before` patches its cache instead of revalidating. The pair
is exact only when nothing else changed the object between them, which, as above,
nothing here can show for a directory. A directory's pre-operation attributes
**MUST** therefore be omitted, which makes the client revalidate. A file's are
returned only when the primary captured them and accepted the operation inside
one hold of the file's **accept lock**: a per-file lock at the primary that every
write, truncate, attribute change and clearing of [§9.6](#9.6%20setuid%20and%20setgid%20are%20cleared%20when%20an%20unprivileged%20caller%20changes%20the%20file) takes before it draws its
`Version` ([§9.4](#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)). The commit serialisation of [§2.5](#2.5%20Where%20%60size%60%20lives) is not enough: it orders commits,
and another client's write can be accepted between a capture and the operation
without committing, so the pair would bracket two changes. Where the primary
cannot take the accept lock — the operation is forwarded, or the lock is
contended past a bound — the pre-operation attributes **MUST** be omitted too.

`Access` **MAY** be omitted, or updated on a coarse schedule. An implementation
that updates it on every read has made every read a write, on a record shared by
every reader of that file. If it is updated at all, the policy **MUST** be
configured, not per-adapter.

### 9.3 Residency is not an attribute

There **MUST NOT** be an attribute, extended attribute or flag reporting whether
a file is local, cached, evicted or remote.

Residency is computed from two oracles at the moment it is asked ([RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function)).
Anything stored is a third answer that can disagree with both, and the first
consumer of it will be something that decides whether to read.

Reporting *allocation* to `SEEK_DATA` and `SEEK_HOLE` is the filesystem
service's, and it **MUST** be answered from [RFC 6](rfc-6-block-metadata.md)'s hole set through the engine
([RFC 8](rfc-8-engine.md)); this component holds no allocation. It **MUST NOT** be answered from
the journal, which cannot tell "never written"
from "no longer held" ([RFC 0 §4.1](rfc-0-data-lifecycle.md#4.1%20The%20two%20oracles)), and **MUST NOT** be implemented by editing
existence ([RFC 6 §3.5](rfc-6-block-metadata.md#3.5%20Operations%20that%20make%20holes)). The same holds for `READ_PLUS` and every other
allocated-range answer, and in all of them a zero ref under a newer overwrite
record counts as data: the overwrite wrote bytes there not yet offloaded, and a
sparse-aware copy told it was a hole would skip them.

### 9.4 The change attribute and ctime never move backward

A client compares the change attribute — or, over NFSv3, `ctime` and `mtime`,
and over SMB the change time — to decide whether its cache still holds. A value
that moves backward, or repeats, keeps a stale cache alive.

- **`Version` is drawn, not counted.** Every change to a file — an attribute,
  flag, ACL, xattr or link change, an accepted write, a directory delta — takes
  its `Version` from the version counter of the journal at the file's primary,
  the counter that orders content writes ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)), when the primary accepts
  the change, holding the file's accept lock ([§9.2](#9.2%20Timestamps)). That lock is the one
  rule for capturing NFSv3 pre-operation attributes; the layers above cite it
  rather than restating it. The counter only rises. Every time a journal opens, and before it
  serves a share it did not serve when it opened, it raises the counter above the
  share's version floor ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)).
- **Every stored `Version` is in the floor.** A change that draws a `Version`
  **MUST** store it in the File in its own transaction — a `chmod` as much as an
  existence commit — and that transaction **MUST** write the file's floor entry
  at the new `Version`, deleting the entry it supersedes ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)). A
  directory delta, which never rewrites the directory's record ([§9.2](#9.2%20Timestamps)), writes
  its own floor entry at its version under its own unique key, and the fold that
  absorbs the delta replaces it with the directory's. The floor is therefore one
  entry per File at its `Version`, plus one per unfolded delta, and covers every
  `Version` the share stores. Without this a `chmod` that draws 101 after a write
  at 100 is stored where no floor sees it; after a crash the counter resumes at
  101 and the next change repeats it, and an NFS client keeps a stale cache. A
  file's `Version` therefore rises with every change across restarts, moves and
  failovers, because the versions it is drawn from do.
- The stored `Version` is the highest drawn for the changes the File records: an
  existence commit sets it to the larger of the stored value and the newest
  version it covers, and a directory's deltas fold by maximum. The overlay
  reports the larger of the stored `Version` and the newest staged write's
  ([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr)).
- **Only server-chosen times are clamped.** `Change` always, and `Modify` or
  `Access` when the server chooses them — set by a write, or by a set-attribute
  to server time — are set to the later of the primary's clock and one
  microsecond past the value the file already has, so a primary whose clock is
  behind its predecessor's never moves them backward. A time the client gives —
  NFS `SET_TO_CLIENT_TIME`, `utimensat` with a time, SMB set-information with a
  time — **MUST** be stored as given, earlier or later than the current value:
  archive, copy and synchronisation tools restore past times, and a
  synchronisation tool whose restored times read back as "now" re-copies every
  file on every run.
  `Change` advances with that set as with every change, content or attribute,
  through every protocol: no open suspends it
  ([RFC 17 §5.1](rfc-17-vfs.md#5.1%20Write)).

The change attribute an adapter reports is `Version`; nothing reports a value
derived from the clock alone.

> decision: an SMB request to suspend change-time updates on an open is accepted
> and not applied to `Change`. NFS clients read `Change` as ctime and validate
> their caches by it, so a suspended `Change` hides SMB writes from them. Keep a
> separate SMB change time, honouring the suspension, if a Windows application is
> shown to depend on it.

### 9.5 An explicit time outlives the writes staged before it

A client that sets `Modify` explicitly — `touch`, a copy or archive tool
restoring a time, SMB's set-information with a time — expects the value to stay
until the next write. Writes staged before the set, not yet committed, would
otherwise commit after it and overwrite it with their own times.

A `SetAttrs` that sets `Modify` or `Access` to a given time **MUST** be applied
through an existence commit ([§2.5](#2.5%20Where%20%60size%60%20lives)) that first commits every write to the file
staged before it and then applies the given times in the same transaction, after
them. Writes staged later advance `Modify` as any write does. Nothing is carried
through the journal: once that commit lands no earlier write remains to overwrite
the value, and recovery re-applies only writes above the file's `Applied`
([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)), all of which came later. The cost is one journal sync and one
transaction per explicit set on a file with staged writes.

### 9.6 setuid and setgid are cleared when an unprivileged caller changes the file

A setuid or setgid program whose bytes change keeps running with the program's
privileges, whoever changed them. Linux clears the bits on every unprivileged
change of content, the owner's included, and POSIX permits it; this component
does the same:

- a write, truncate, deallocate, clone or copy into a regular file by a caller
  that is not privileged ([§7.4](#7.4%20The%20identity%20arrives%20resolved)) — the file's owner included — **MUST** clear the
  set-user-ID bit, and the set-group-ID bit when group execute is set (without
  group execute the bit marks mandatory locking, and stays);
- a change of a regular file's owner or group **MUST** clear the same bits, in the
  transaction that makes it, whoever makes it.

The clearing is a mode change with its own `Version` ([§9.4](#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)), and **MUST**
commit before the write that causes it is staged: staged first, the rewritten
program would stay executable with its privileges until the next existence
commit. The filesystem service makes it at the file's primary before handing the
write, clone or copy to the engine ([RFC 17 §5.1](rfc-17-vfs.md#5.1%20Write)): one transaction for the first such
change, none after, since the bits are then clear. A privileged caller's write
leaves the bits.

## 10. Invariants

N33–N35 sit after N25, with the rules they belong beside, rather than in number
order; the IDs are kept as issued, because other RFCs and tests cite them.

| # | Invariant |
| --- | --- |
| N1 | A file's `Nlink` equals the number of entries naming it, changes in the transaction that changes them, and fails the transaction rather than going negative. |
| N2 | A file is released when, and only when, it has a pending-release record, `Nlink` is zero and no open state references it; only the removal of its last entry, or its base's release for a named stream, writes that record, so a share root and a live file's streams are never released. Nothing else keeps a file alive. |
| N3 | The transaction that removes a file's last entry writes its pending release, always, holding no holder list; only the release transaction — the engine's `Release`, which drops the refs — deletes it, so a restart or a new primary resumes it. Holders of an unlinked file are its durable opens ([RFC 14](rfc-14-open-state.md)); open state of a linked file is never written. |
| N4 | A rename applies wholly or not at all, and its loop check is evaluated inside its transaction and guards every ancestor it reads. Create, link and rename-into guard the parent; removing a directory writes it. |
| N5 | A handle names a file and a share, is stable across restart, and resolves to stale — never to another file and never to "not found" — when its file is gone. A `FileID` is unguessable and never reissued; so is a principal's ID. |
| N6 | No name, path or parent appears in a handle, a lock, a ref or a journal key. |
| N7 | Every permission decision is made in this component, against the file the operation will act on, and a grant made at open is computed and evaluated here rather than in an adapter. |
| N8 | A cached authorisation or identity is keyed by every field it was derived from, the share grant included; the grant is evaluated on every call. |
| N9 | A release verifies inside its transaction that `Nlink` is zero, its pending release exists and no durable open names the file or a stream of it; an open of a file that has lost its last entry (for a stream, whose base has) guards that File record; a file's streams are released with it, each as a file. |
| N10 | Every wait in this component ends without operator action. |
| N11 | A listing is ordered by a 63-bit keyed digest of each entry's key; its cookie is that digest, at least 3 with the top bit clear, and resolves with no stored state; no page ends inside a collision chain. |
| N12 | `Size`, `Charged`, `Applied`, a file's `Modify` and its write `Change` are fields of the File, stored once, written only by an existence commit, which also advances `Version`; a directory's `Modify`, `Change` and `Version` advance instead by entry deltas folded into its record (§9.2); `GETATTR` is `Files.Get` with the engine's overlay applied by the filesystem service. This component calls no other component. |
| N13 | An entry is its own record, and no operation's cost grows with the size of its directory beyond the results it returns. |
| N14 | Residency is not an attribute. |
| N15 | `Mode` and the ACL agree after every transaction; `chmod` merges into the ACL as [RFC 8881 §6.4.1.1](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4.1.1) specifies and never replaces it. |
| N16 | A principal's usage is the sum of its files' `Charged`: each file's logical bytes, holes excluded, in full whether shared or not, unchanged by deduplication, compression or GC. |
| N17 | On a case-insensitive share an entry is keyed by its folded name under the share's recorded fold rule, and stores the name as given. |
| N18 | A file's numeric id is its stored `Number`: allocated at create, never changed, never reused in its share; every protocol's numeric id is derived from it and nothing resolves a number to a file. |
| N19 | An exclusive create stores its verifier in `CreateVerifier`, never in a time; a retry with the same verifier succeeds until the first `SetAttrs` clears it. |
| N20 | A file has only the names its entries hold: no short name is generated. |
| N21 | A directory's change info is reported with `atomic` false. |
| N22 | A create sets `Owner` to the caller's principal and `Group` to the credential's group, the principal's primary group or, with neither, the caller's own principal, or to the parent's group when the parent is setgid; a directory created under a setgid parent is setgid; a non-directory keeps setgid at create or `chmod` only when the caller is in its group or privileged; no fixed default group is ever assigned. |
| N23 | `Version` is drawn from the counter of the journal at the file's primary, stored in its own transaction with a floor entry at that `Version` for every change, attribute-only ones included, and never moves backward or repeats across restarts, moves and failovers; `Change` and server-chosen times never move backward; a client-given time is stored as given; no open suspends `Change`. |
| N24 | An explicit `Modify` or `Access` is applied in an existence commit after every write staged before it. |
| N25 | An unprivileged caller's write, truncate, deallocate, clone or copy into a regular file — the owner's included — and any change of its owner or group, clears setuid, and setgid with group execute, before the change is staged. |
| N33 | Immutable and append-only flags are enforced at the chokepoint for every caller and protocol, and only a privileged caller sets or clears them. |
| N34 | A file's NFSv3 pre-operation attributes are reported only when captured under the file's accept lock in one hold with the operation. |
| N35 | A delete-pending directory takes no create, link or rename into it. |
| N26 | A rename replaces a directory only with a directory, and only an empty one, which it proves empty and writes in its own transaction; a non-directory replaces only a non-directory. |
| N27 | Removing, renaming and linking are authorised inside their own transactions, against both directories and the file. |
| N28 | A handle carries no shard; a `FileID` is on no wire outside a handle. |
| N29 | A share's name rule is enforced for every protocol; neither a stored name nor a key is normalised, and a case-insensitive key is folded unit for unit by the 16-bit upcase table its fold rule records by ID. |
| N30 | A restore that keeps numbers raises the allocator above every number it imports; a snapshot's 64-bit id carries its ordinal. |
| N31 | A delete on close unlinks as the principal of the open that set the mark, authorised by that open's grant. |
| N32 | Every attribute a client sees is the join of the File and the overlay read under the file's commit serialisation; a directory's NFSv3 pre-operation attributes are never reported. |

## 11. API surface and observability

### 11.1 Interface

Signatures are indicative; the obligations are normative. Every call made on a
client's behalf about a file takes the resolved identity ([§7.4](#7.4%20The%20identity%20arrives%20resolved)) and is authorised
inside this component ([§7.1](#7.1%20One%20chokepoint)). Four take none: `PendingReleases` (recovery) and
`Resolve` (routing a handle to its file) act for no client, and `Info` and
`Usage` report share-wide counts that the filesystem service returns only to a
caller its share grant admits, checked on the call that asked for them.
These are the filesystem service's views ([RFC 17](rfc-17-vfs.md)); adapters never hold them.
How they are assembled into one store is [RFC 16](rfc-16-metadata-store.md)'s.

```go
type Namespace interface {
	// Names (§3). Lookup and the listings never enumerate more than they return.
	Lookup(ctx context.Context, id Identity, dir Handle, name []byte) (Handle, File, error)
	Entries(ctx context.Context, id Identity, dir Handle, after Cursor) iter.Seq2[Entry, error]
	EntriesPlus(ctx context.Context, id Identity, dir Handle, after Cursor) iter.Seq2[EntryFile, error] // §3.4
	Create(ctx context.Context, id Identity, dir Handle, name []byte, a Attrs) (Handle, File, error) // guards dir (§3.6)
	Link(ctx context.Context, id Identity, dir Handle, name []byte, target Handle) error // refuses a target with Nlink zero (§4.5)
	// Unlink and Rename report a file whose last entry they removed; its
	// pending release is already written (§4.3), and the filesystem service
	// runs the release once no open holds it.
	Unlink(ctx context.Context, id Identity, dir Handle, name []byte) (Orphaned, error)                                // §4
	Rename(ctx context.Context, id Identity, from Handle, fromName []byte, to Handle, toName []byte) (Orphaned, error) // §5
	PendingReleases(ctx context.Context, share ShareID) iter.Seq2[FileID, error] // recovery (§4.3)
}

// Orphaned names the file an operation left with no entry; zero when none.
type Orphaned struct{ File FileID }

type Files interface {
	Get(ctx context.Context, id Identity, h Handle) (File, error) // GETATTR: cost as §2.5 states
	SetAttrs(ctx context.Context, id Identity, h Handle, a Attrs) (File, error)
	ACL(ctx context.Context, id Identity, h Handle) (ACL, error) // synthesised from Mode if none stored (§2.6)
	SetACL(ctx context.Context, id Identity, h Handle, acl ACL) (File, error)
	Xattrs(ctx context.Context, id Identity, h Handle) iter.Seq2[Xattr, error]
	SetXattr(ctx context.Context, id Identity, h Handle, x Xattr) error
	RemoveXattr(ctx context.Context, id Identity, h Handle, name []byte) error
	Streams(ctx context.Context, id Identity, h Handle) iter.Seq2[File, error] // §2.7
	Authorize(ctx context.Context, id Identity, h Handle, want Access) error  // share grant, then mode or ACL, every call (§7.2, §7.4); rights of §7.7
	Root(ctx context.Context, id Identity, share ShareID) (Handle, File, error) // authorised against the share grant (§7.4)
	Resolve(ctx context.Context, h Handle) (FileID, error)                     // ErrStale, ErrWrongShare
}

type Capacity interface {
	Info(ctx context.Context, share ShareID) (FilesystemInfo, error)
	Usage(ctx context.Context, share ShareID, p Principal) (PrincipalUsage, error) // rquota, SMB quota
}

// Attrs names the fields a SetAttrs changes; unset fields are untouched.
// Size and Modify here are a client's explicit set, applied through an
// existence commit, never written to the File directly (§2.5).
type Attrs struct {
	Mask                  AttrMask
	Owner, Group          Principal
	Mode                  uint32
	Flags                 FileFlags
	Birth, Access, Modify time.Time
	Size                  int64
}

// Events is the hook the filesystem service supplies (§7.6). Emit never
// blocks; an event it cannot take is dropped and counted.
type Events interface {
	Emit(e Event)
}

var (
	ErrStale        = errors.New("namespace: stale handle") // §6.3
	ErrWrongShare   = errors.New("namespace: handle of another share")
	ErrInconsistent = errors.New("namespace: nlink underflow") // §4.1
	ErrCrossProject = errors.New("namespace: rename or link across tree quotas") // §2.8
)
```

A serialisation conflict is retried inside the call under the caller's deadline
and never returned ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)).

This component declares no interface on the engine or on open state: `Events` is
a sink, not a dependency, and a store built with none emits nothing. The joins
that need content or open state are the filesystem service's ([§1](#1.%20Purpose)).

### 11.2 Observability

Metric names are shown without the deployment's prefix.

| Answers | Metric | Type |
| --- | --- | --- |
| handles resolved stale ([§6.3](#6.3%20Staleness%20is%20reported%2C%20never%20guessed)) | `namespace_stale_handles_total` | counter |
| `Nlink` underflows ([§4.1](#4.1%20%60nlink%60%20is%20exactly%20its%20entries)); any nonzero value is an alert | `namespace_nlink_underflow_total` | counter |
| pending-release records held ([§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees)) | `namespace_pending_releases` | gauge |
| releases, labelled `result`; a failure is retried, not dropped | `namespace_releases_total` | counter |
| audit and quota events dropped by a slow sink ([§7.6](#7.6%20Decisions%20are%20observable)) | `namespace_events_dropped_total` | counter |
| conflicts retried, by `op` | `namespace_conflict_retries_total` | counter |

No metric carries a share or principal label: at 10⁴ shares a share label
multiplies every series by 10⁴. Per-share figures are read through the
management API ([RFC 16](rfc-16-metadata-store.md)). Operation counts and latency are the filesystem
service's (`vfs_op_seconds`), and quota refusals are counted where the
quota is enforced, both in [RFC 17](rfc-17-vfs.md); this component does not count them again.

An `Nlink` underflow logs the file at `Error`. A stale handle is routine for
clients and logs at `Debug`.

## 12. Conformance

Every check runs against every backend through one shared conformance suite, in
the tiers and under the rules of the [index](rfc-index.md). Checks go through the
interfaces of [§11.1](#11.1%20Interface) and never read a key.

### 12.1 Group A — wrong file, lost file, wrong caller

| Requirement | Check |
| --- | --- |
| [§4.1](#4.1%20%60nlink%60%20is%20exactly%20its%20entries) `Nlink` | Over random interleavings of create, link, unlink and rename-over, assert `Nlink` equals the entries naming the file after every transaction. |
| [§4.2](#4.2%20Open%20state%20is%20the%20second%20holder) open-unlinked | Open a file, unlink it, read through the handle. Assert the content is served and the refs are still counted. Close, assert release. |
| [§4.4](#4.4%20There%20is%20no%20third%20holder) no third holder | Delete every hold list. Assert the open-unlinked and snapshot checks still pass. A suite that passes only with the list present is testing the list. |
| [§5.1](#5.1%20One%20transaction) rename atomicity | Crash between the two entry writes. Assert the file is visible at exactly one name. |
| [§5.2](#5.2%20The%20loop%20check%20is%20inside%20the%20transaction) loop check | Rename A under B and B under A concurrently, on the backend that validates no reads. Assert one fails and no unreachable cycle exists. Remove the ancestor guards: the check fails. |
| [§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on) rmdir against create | Remove directory D while creating in it, repeatedly, on the backend that validates no reads. Assert exactly one commits and no entry or file is left under a deleted directory. Remove the rmdir's write of D: the check fails. |
| [§6.3](#6.3%20Staleness%20is%20reported%2C%20never%20guessed) staleness | Release a file, create 10⁶ more, resolve the old handle. Assert stale, not another file and not "not found". |
| [§6.1](#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path) restore does not alias | Restore a share into a new share. Assert no handle from the original resolves in the restored one, and a handle from the restored one is refused by the original. |
| [§7.2](#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always) grant ownership | Grant an open through one adapter, then reach the same file through the other. Assert the other adapter observes the grant. A single-adapter rig cannot fail this. |
| [§7.3](#7.3%20The%20decision%20is%20against%20the%20file%2C%20not%20the%20name) file check | Look a name up, replace the entry, then act. Assert the check ran against the file acted on. |
| [§7.5](#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read) cache key | Two identities differing only in a field the key omits. Assert the second is not served the first's decision. A test that adds no field to the identity cannot fail. |
| [§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees) pending release | Open a file, unlink it, crash. Assert the refs are still counted until grace ends, and released after it unless the open was reclaimed. Then unlink a closed file and crash before its release runs: assert the record survives and recovery releases it. |
| [§4.2](#4.2%20Open%20state%20is%20the%20second%20holder) lazy open state | Open and close a linked file 10⁴ times. Assert no namespace record was written. |
| [§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees) release through the engine | Unlink a file the journal still holds dirty. Assert the refs are dropped and the journal holds nothing of it. |
| [§2.5](#2.5%20Where%20%60size%60%20lives) `Change` | `chmod` a file, then write it; then write it and `chmod` it. Assert `GETATTR`'s `Change` is the later change both times. |
| [§2.5](#2.5%20Where%20%60size%60%20lives) overlay | Write past EOF without committing, `GETATTR` through the filesystem service, crash, recover. Assert `GETATTR` showed the new size before the crash and the committed size and holes agree after it. |
| [§2.5](#2.5%20Where%20%60size%60%20lives) `Version` on write | Write and commit a file with no attribute change. Assert `Version` advanced, both through the overlay before the commit and in the File after it. |
| [§2.4](#2.4%20Attributes%2C%20and%20who%20writes%20them) owner and group at create | Give a user a primary group and one other membership. Create a file over NFS with a credential whose gid is the other membership, and one over SMB. Assert both `Owner`s are the user, the NFS file's `Group` is the credential's gid and the SMB file's the primary group, never a fixed default. Give a directory a third group and the setgid bit; create a file and a directory in it over each protocol; assert all four take the directory's group and both new directories are setgid. A build that ignores the credential's gid fails the first assertion; one that ignores setgid fails the second. |
| [§2.6](#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode) `chmod` merge | Set an ACL holding a named-user allow entry granting write, an inherit-only entry, and an entry both effective and inheritable; `chmod 600`. Assert the named user is granted nothing, the inherit-only entry is unchanged, the other is split into an unchanged inherit-only copy and a limited effective one, and `Mode` equals the ACL's implied mode. |
| [§2.7](#2.7%20Extended%20attributes%20and%20named%20streams) stream identity | Create a stream, `chown` its base file, release the base. Assert the stream is checked against the new owner, reports the base's file id, and is released with it. |
| [§2.8](#2.8%20A%20share%20is%20one%20filesystem) root | Resolve `..` at a share's root. Assert it is the root. |
| [§2.8](#2.8%20A%20share%20is%20one%20filesystem) logical charging | Write the same content into two files owned by two users, clone one of them to a third user, let dedup and GC run. Assert each user is charged the full logical size. Punch a hole and truncate: assert `Charged` and usage fall by the bytes removed. `chown`: assert exactly `Charged` moves. |
| [§2.8](#2.8%20A%20share%20is%20one%20filesystem) soft quota | Exceed `Soft`, write within `Grace`, then after it. Assert the first succeeds, the second is refused, and an event was emitted at each crossing. |
| [§7.4](#7.4%20The%20identity%20arrives%20resolved) share grant | Remove a principal's grant while it holds cached handles and a cached authorisation. Assert its next call on each handle is refused. |
| [§3.3](#3.3%20Case) case | On a case-insensitive share, create `README`, then `readme`. Assert the second conflicts, a lookup of `ReadMe` reads one key, and a listing returns `README` as given. |
| [§3.5](#3.5%20A%20cookie%20survives%20concurrent%20mutation) cookie | Delete an entry before the cursor mid-listing. Assert no untouched entry is skipped or repeated. Then evict every cached cookie and assert the listing resumes rather than restarting. |
| [§6.5](#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused) file id | Create and release 10⁶ files across two shards, failing one primary over mid-range and restarting. Assert no number was issued twice, released ones included, and that a surviving file reports the same NFS and SMB ids after the restart, the failover and a move to another shard. A derivation from the `FileID` or the handle fails the first assertion; a counter held in memory fails the second. |
| [§2.10](#2.10%20Exclusive%20create) exclusive create | Create exclusively, drop the reply, retry with the same verifier: assert success and the same file. Retry with another verifier: assert "exists". `SETATTR`, retry with the first: assert "exists". Assert no time attribute ever read back as the verifier. Repeat the first retry across a failover. |
| [§3.7](#3.7%20No%20short%20names) short names | Create a long name over SMB; query its alternate name and open its 8.3 form. Assert not supported, and not found. |
| [§9.2](#9.2%20Timestamps) change info | 64 clients create in one directory. Assert every reply's change info has `atomic` false and `after` greater than `before`. |
| [§5.2](#5.2%20The%20loop%20check%20is%20inside%20the%20transaction) loop, sequential | Rename a directory under its own child with no concurrency at all. Assert refusal. This is the one check that fails a build with no loop check on every backend, independent of how the backend detects conflicts. |
| [§6.1](#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path) one spelling | For each handle, derive every other byte form the decoder's underlying parser accepts. Assert each is refused, or resolves to the same file and compares equal, and that rename and locking through the alias behave as through the original. |
| [§4.5](#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction) release race | Unlink a closed file; between the release's decision and its transaction, open the file by handle. Assert the release aborts, the open reads the content, and the release runs at the open's close. On the backend that validates no reads, remove the open's guard: the check fails. Link the unlinked file by handle: assert stale. |
| [§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees) streams released | Give a file two streams with content; open one stream; unlink the base. Assert nothing is released. Close the stream: assert the base and both streams are released, every one of their refs dropped, and no stream's File remains. A release that drops only the base's refs leaks the streams'. |
| [§3.5](#3.5%20A%20cookie%20survives%20concurrent%20mutation) cookie resolves | List a directory of 10⁵ entries in pages, deleting between pages the entry each cookie came from, and sending each page's request to a different node. Assert every untouched entry is returned once, every cookie is at least 3 with its top bit clear, and no cookie state was stored. With a test hash key forcing a chain of three, assert no page ends inside it. |
| [§5.4](#5.4%20Rename%20over%20an%20existing%20entry) rename over an entry | Rename a directory over an empty directory: assert success and the target released. Over a non-empty one, a directory over a file, a file over a directory: assert each refused. On the backend that validates no reads, race a create into the target with the rename; assert one fails and no entry survives under a released directory. Rename a file onto another link of itself: assert nothing changes. |
| [§7.7](#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs) removal rights | Over NFS and over SMB: remove a file with delete-a-child on its directory and no right on the file, and with delete on the file and no right on the directory; assert both allowed and refused with neither. In a sticky directory governed by mode, remove another's file in another's directory: assert refused. Move a directory to another parent without write on it: assert refused. A build that authorises removal with one `Authorize` on the directory fails the second case. |
| [§3.8](#3.8%20Names%20every%20protocol%20of%20a%20share%20can%20use) names | On a `windows` share create `CON`, `aux.txt`, `a?b` and `dir.` over NFS: assert each refused as an invalid name. On a `posix` share assert each is created. On a case-insensitive share create `Straße.docx`, then `STRASSE.docx`: assert two entries; create `ǅ` and look up `ǆ`: assert one entry, found with one read, listed as given. Create `K.txt` and look up the same name spelled with the Kelvin sign U+212A: assert not found, as NTFS answers; a build that applies Unicode case folding finds it. Create `café` decomposed over NFS and composed over SMB: assert two entries. A build that folds fully or normalises the key fails the first assertion. |
| [§6.1](#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path) no shard, no `FileID` | Mint a file's handle, move the file to another shard, look it up again: assert the two handles are byte-identical. Read every identifier each protocol reports for the file; assert none contains its `FileID`. |
| [§6.5](#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused) restored and snapshot ids | Restore a share keeping numbers, then create a file: assert its number exceeds every restored one. Take two snapshots of one file: assert their 64-bit ids differ from each other and from the live file's, over NFS and SMB. |
| [§8.2](#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later) delete on close identity | alice opens a file with delete on close; bob, with no right to delete it, holds a second open and closes last. Assert the file is unlinked, as alice. Repeat with alice's lease expiring before bob closes: assert it is unlinked, as alice. A build that unlinks as the last closer fails bob's close-time unlink. |
| [§2.5](#2.5%20Where%20%60size%60%20lives) join reads one state | Commit a file's writes in a loop while 16 readers `GETATTR` it. Assert no reader sees a `Size` or `Version` lower than one it saw before. A join that reads the File and the overlay without the sequence check fails. `LOOKUP` a file with uncommitted writes past its end: assert the attributes returned carry the new size. |
| [§9.2](#9.2%20Timestamps) NFSv3 pre-operation attributes | Create in a directory over NFSv3 while another client creates in it. Assert no reply carries the directory's pre-operation attributes. |
| [§9.4](#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward) attribute version survives a crash | Write and commit at version 100; `chmod` the file, reading its `Version`; kill the process before any further journal record; restart; `chmod` again. Assert the second `Version` exceeds the first. A floor that covers only refs and `Applied` returns the first value again. |
| [§9.4](#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward) client time kept | `SETATTR` `mtime` to 2001-01-01 with `SET_TO_CLIENT_TIME`, `touch -d` a past time, and SMB set-information with a past time, each on a file whose `Modify` is now. Assert `GETATTR` returns each given time and `Change` advanced. A build that clamps every time returns now. |
| [§9.4](#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward) change never moves back | Stage three writes, reading `Version` after each; commit; `chmod`; restart; write; fail the shard over to a node whose clock is an hour behind; write and `chmod` again. Assert `Version` rises strictly at each change and `Change` and `Modify` never fall. Remove the clamp: `Change` falls after the failover. Count `Version` per file instead of drawing it: a commit of three writes moves it below what the overlay reported. |
| [§9.5](#9.5%20An%20explicit%20time%20outlives%20the%20writes%20staged%20before%20it) explicit time | Stage a write, `SETATTR` `mtime` to a past time over NFS, then `COMMIT`; repeat over SMB with set-information on a second handle. Assert `GETATTR` returns the set time after the commit and after a crash and recovery; write again, assert `Modify` advanced. A build that writes the set time to the File directly returns the staged write's time after the commit. |
| [§9.6](#9.6%20setuid%20and%20setgid%20are%20cleared%20when%20an%20unprivileged%20caller%20changes%20the%20file) setuid cleared | Make a file mode 6755 owned by alice; write to it as bob. Assert both bits are clear before the write's reply. Repeat with mode 2745: assert setgid kept. `chown` a 4755 file: assert setuid cleared. Write as alice, the owner: assert both bits cleared. Truncate, deallocate, clone into and server-side copy into a 6755 file as alice: assert each clears them. Write as a privileged caller: assert the bits kept. A build that exempts the owner fails the alice write; one that clears in the existence commit shows the bits set between the reply and the commit. |
| [§2.4](#2.4%20Attributes%2C%20and%20who%20writes%20them) setgid for a non-member | In `/srv/drop`, mode 3777, group `finance`, create a file with mode 2755 as a user outside `finance`: assert the file's group is `finance` and setgid clear. Repeat as a member: assert setgid kept. Create a directory there as the non-member: assert it is setgid. `chmod 2755` a file of group `finance` as its non-member owner: assert setgid clear. A build that inherits the bit unchecked fails the first assertion. |
| [§2.4](#2.4%20Attributes%2C%20and%20who%20writes%20them) no group at all | Create a file over SMB as a principal with no primary group, and over NFS as one whose credential gid maps to no principal and who has no primary group. Assert both creates succeed and the file's `Group` is the creator's own principal. |
| [§7.8](#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint) flags | Set immutable on a file as a privileged caller; as its owner, over NFS and SMB, write, truncate, `chmod`, rename, link and unlink it, and write through an open taken before the flag was set: assert each refused with a permission error. Set append-only: assert an append succeeds and an overwrite, a truncate and an unlink are refused. Clear either flag as the unprivileged owner: assert refused. A build that stores the flags without checking them fails every assertion. |
| [§9.2](#9.2%20Timestamps) file pre-operation attributes | Two NFSv3 clients write one file in a loop at its primary. Assert every reply that carries pre-operation attributes has `before` equal to the attributes after the change immediately preceding it in the file's `Version` order. Capture under the commit serialisation only: the check fails. |
| [§8.2](#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later) delete-pending directory | Mark a directory delete on close over SMB; while the mark stands, create in it over SMB and over NFS, and rename a file into it. Assert each refused, and the directory removed at close. Race a create past the check: assert the close-time unlink fails as not empty and is reported. |
| [§4.1](#4.1%20%60nlink%60%20is%20exactly%20its%20entries) name freed | Unlink a name and create it again as soon as the unlink is acknowledged, on every backend including a slow remote one. Assert the create never sees the name as taken and the parent's `Version` moved. |

### 12.2 Group B — cost

| Requirement | Check |
| --- | --- |
| [§2.2](#2.2%20Entry) entry records | Create *N* entries in one directory. Assert records **written** per create is constant in *N*. A correctness assertion on the resulting listing passes a quadratic implementation. |
| [§3.1](#3.1%20Lookup%20resolves%20a%20name%20to%20a%20file%2C%20and%20that%20is%20all%20it%20does) lookup | Assert records **read** per lookup grow at most logarithmically in *N*. |
| [§3.4](#3.4%20Enumeration) listing | Assert records read per page are bounded by the page size, and that `EntriesPlus` issues batched reads, not one per entry. |
| [§2.5](#2.5%20Where%20%60size%60%20lives) `GETATTR` | Assert one record read for a regular file with no uncommitted writes, and for a directory with no unfolded delta. |
| [§9.2](#9.2%20Timestamps) directory times | 64 clients create in one directory. Assert no transaction reads the directory's record to update its times. |
| [§6.4](#6.4%20Resolution%20does%20not%20touch%20the%20namespace) resolution | Assert handle resolution reads no entry record, at any path depth. |
| [§1](#1.%20Purpose) leaf | Assert the namespace component imports no engine, journal, block-metadata or open-state component. |
| [§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on) shared guards | 64 clients create in one directory on each backend. Assert creates/s is within 20% of creates into 64 directories, so guards did not serialise them. |

### 12.3 What must not stand in

- **A single-client rig MUST NOT stand in for [§3.5](#3.5%20A%20cookie%20survives%20concurrent%20mutation).** The failure needs two
  clients on one directory at once.
- **A single-adapter rig MUST NOT stand in for [§7.2](#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always).** A grant that only one
  protocol can see passes every test that only speaks that protocol.
- **A read-validating backend MUST NOT stand in for [§5.2](#5.2%20The%20loop%20check%20is%20inside%20the%20transaction) or [§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on).** There,
  the ancestor reads and the emptiness scan already conflict, so a build that
  forgot the guards passes. The concurrent checks run on the backend that
  validates no reads, where only a guard makes the conflict.
- **A concurrent rig MUST NOT stand in for the sequential loop check.** The
  concurrent case exercises the guards; only the sequential one isolates the
  check itself.
- **A correctness assertion MUST NOT stand in for [§2.2](#2.2%20Entry).** A quadratic directory
  returns the right listing.

### 12.4 Benchmarks and targets

| Benchmark | Measures | Target |
| --- | --- | --- |
| Lookup in directories of 10² to 10⁷ entries | p99 latency | ≤ 100 µs at 10⁷, within 2× of the 10² figure |
| Create into directories of 10² to 10⁷ entries, 64 clients | creates/s, records written per create | records written constant; creates/s within 20% across sizes |
| Create into one directory, 64 clients | creates/s, retries per create | retries per create near zero ([§9.2](#9.2%20Timestamps)) |
| List a page of 1,000 entries | p99 latency | ≤ 5 ms at any directory size |
| `GETATTR` | p99 latency | one record read; ≤ 1.3× a raw read of the File record |
| Handle resolution at path depth 1 and 64 | p99 latency | ≤ 50 µs, independent of depth |
| Rename across directories of 10⁶ entries | p99 latency | ≤ 5 ms |

## 13. Open questions

1. ~~**What a case-insensitive share should cost.**~~ Closed: the entry key is
   the folded name and the value holds the name as given ([§3.3](#3.3%20Case)); no second
   index is written.
2. **`Access` cost.** [§9.2](#9.2%20Timestamps) allows a coarse schedule and does not choose one.
   Whether any consumer reads `Access` at all is unmeasured; if none does,
   omitting it removes a write from the read path.
3. ~~**Placing a new file near its parent.**~~ Closed: `FileID`s are unguessable
   ([§6.1](#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)); placement near the parent returns only with MACed handles.
4. **Where a recycle bin sits.** Nothing below the namespace has to know a
   file is in a bin, so it is not engine policy
   ([RFC 8 §1.2](rfc-8-engine.md#1.2%20Non-goals)); whether it is built here or above this component is
   open. The constraints it keeps wherever it sits are [§5.5](#5.5%20A%20recycle%20bin%20is%20a%20rename%2C%20and%20keeps%20three%20constraints)'s.
5. **A reverse-name index** — a file's names, for repair and auditing — is not
   written and no key is reserved for it; add one, at one write per create, link
   and rename, when a consumer needs it.

## Appendix A — where the current code differs

Descriptive, for the refactor. None is a rule to build around, and a difference
**MUST NOT** be closed by amending the requirement. The Entry/File split of
[§2](#2.%20The%20entities) already holds: File records keyed by `FileID`, entries by parent and name, no
stored path, cursor-paged listing.

| Requirement | Code today |
| --- | --- |
| [§2.4](#2.4%20Attributes%2C%20and%20who%20writes%20them) the offload commit writes no namespace record | every offload rewrites the file's attributes with the refs |
| [RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation) (I7) | on one backend the file's attribute value embeds extended attributes with no cap on their count, and can outgrow what the store reclaims |
| [§2.5](#2.5%20Where%20%60size%60%20lives) `size` stored once, written only by existence | `size` has three sources reconciled at run time and is grown from the journal at every share start |
| [§2.1](#2.1%20File) protocol-neutral File | uid and gid on the file; SMB attributes and SIDs carried beside them |
| [§2.6](#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode) `Mode` and ACL agree | the ACL records how it was created and which protocol set it last |
| [RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries) (I8) conflicts retried under the caller's deadline | retried under a fixed budget, after which a conflict reaches the client as an I/O error |
| [§1](#1.%20Purpose) the store is a leaf | one transaction type spans namespace and content records |
| [§9.3](#9.3%20Residency%20is%20not%20an%20attribute) residency is not an attribute | a content-derived identity is a File-record column with a unique index |
| [§4.1](#4.1%20%60nlink%60%20is%20exactly%20its%20entries) one `Nlink` | a second, silently dropped copy on the attribute struct |
| [§4.3](#4.3%20Release%20is%20what%20block%20metadata%20sees) release through the engine, recorded | adapters release refs best-effort outside the transaction; no pending-release record |
| [§4.4](#4.4%20There%20is%20no%20third%20holder) no third holder | open-but-unlinked files are protected by a hold list read by GC |
| [§5.1](#5.1%20One%20transaction), [§9.2](#9.2%20Timestamps) directory times in the transaction | parent directories' timestamps are coalesced outside the transaction and can be lost |
| [§5.2](#5.2%20The%20loop%20check%20is%20inside%20the%20transaction) loop check | inside the transaction, but not serialisable on one backend's isolation level |
| [§3.6](#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on) directory guards across nodes | a directory's removal and a create, link or rename into it are kept apart by a lock inside one metadata service instance, so the guarantee holds within one instance only, not across nodes |
| [§6.1](#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path) opaque, one spelling | a plaintext share-and-UUID string, accepting several spellings of one UUID |
| [§6.3](#6.3%20Staleness%20is%20reported%2C%20never%20guessed) stale, not missing | a released file resolves as not found |
| [§6.5](#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused) numeric file id | a truncated hash of the handle, no collision check |
| [§3.2](#3.2%20A%20name%20is%20bytes%2C%20and%20it%20is%20validated%20at%20the%20boundary) validation at the boundary | duplicated in protocol handlers; the share's maximum is not consulted |
| [§3.3](#3.3%20Case) case | the unique key is byte-exact on a case-insensitive share |
| [§3.1](#3.1%20Lookup%20resolves%20a%20name%20to%20a%20file%2C%20and%20that%20is%20all%20it%20does) lookup is not an enumeration | a case-insensitive miss scans the directory |
| [§3.5](#3.5%20A%20cookie%20survives%20concurrent%20mutation) the cookie is an ordering key | a hash held in a bounded in-process cache |
| [§7.2](#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always) the grant's owner | one adapter computes and stores the open-time grant and passes it back as bypass flags |
| [§9.2](#9.2%20Timestamps) `Modify` reflects the content change | frozen per file while writes are pending, so a second client's overwrite may not advance it |
