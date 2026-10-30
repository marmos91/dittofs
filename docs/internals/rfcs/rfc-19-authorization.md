---
rfc: 19
title: "RFC 19 — authorization"
component: authorization
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-18-identity]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-21-nfs]]"
  - "[[rfc-22-smb]]"
  - "[[rfc-23-control-plane]]"
  - "[[rfc-25-observability]]"
aliases:
  - RFC 19
  - authorization
tags:
  - rfc
---
# RFC 19 — authorization

**Status:** draft. [§10](#10.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone changing how a permission is decided, how `chmod` and an
ACL agree, or how a new file inherits its parent's entries.

| | |
| --- | --- |
| **This RFC** | what a resolved caller may do to a file: the entries of an ACL, the rights, the order one decision runs in, the share ceiling, what privilege changes in a decision, how the mode and the ACL agree, and inheritance |
| **Not this RFC** | who the caller is, the well-known principals and who each one matches, squashing and the caller's flags ([RFC 18 §2.1](rfc-18-identity.md#2.1%20Kinds%20and%20well-known%20principals), [§6](rfc-18-identity.md#6.%20Resolution%20per%20share)); the ACL record, the one chokepoint and when it runs ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode), [§7](rfc-7-namespace-metadata.md#7.%20Permissions)); how a protocol spells an ACL, a SID or an owner string ([RFC 21 §11](rfc-21-nfs.md#11.%20Translation), [RFC 22 §11](rfc-22-smb.md#11.%20Security%20descriptors)); the mapping of a protocol's generic rights ([RFC 22 §16.3](rfc-22-smb.md#16.3%20Namespace)) |

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix B](#Appendix%20B%20%E2%80%94%20where%20the%20current%20code%20differs) says where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

[RFC 18](rfc-18-identity.md) answers **who** the caller is. This RFC answers
**what that caller may do to a file**.

Every file has one access-control list (ACL): an ordered list of entries, each
allowing or denying some rights to someone. A file without a stored ACL still
has one: the one its POSIX mode implies. Windows clients read and write that
list as a *security descriptor*; NFSv4 clients as the `acl` attribute. Both are
spellings of the same stored list. The rights — read data, write data, delete,
change the ACL — have the same bit values in both protocols; what differs is how
a principal is named and where three flag bits sit.

One decision runs in four phases. **Limit**: look up the most this caller may
ever have on this share, the *ceiling*. **Collect**: gather what the caller is
granted, from privilege, which grants everything, or else from the owner's
automatic rights plus the ACL, then from the parent directory, which can grant
delete. **Remove**: the immutable and append-only flags, and the read-only
marker, take away what they forbid. **Cut**: keep only what fits under the ceiling. The request succeeds when
everything it asked for survives.

**The problem, with one example.** alice opens the Security tab on
`\\dittofs\builds\release` from alice-pc and adds three entries: deny bob
write, allow herself full control, allow the `builders` group modify. build01,
a member of `builders`, reaches the same directory over NFSv4.

Suppose each adapter translated on its own. The NFS adapter meets an entry for a
principal it has no `user@domain` name for, and leaves it out of the `acl` it
returns. An operator on build01 runs `nfs4_setfacl -a` to add one more entry; the
tool reads the ACL, appends, and writes the whole list back. bob's deny is gone,
and nothing failed.

With this RFC both adapters call one translation (§5), every principal has a
spelling in both protocols (§5.2), the entries are stored in the order alice's
client sent them (§2.7), and bob's write over either protocol runs the same
walk (§3.1) and is refused.

![One stored ACL shown two ways, as an SMB security descriptor with SIDs and as an NFSv4 acl with who strings, through one translation library both adapters call. bob's write is refused by the same Authorize decision over both protocols, and the three flags whose bits differ are mapped by name](img/rfc19-two-views.svg)

**The words you need.**

- **entry** — one statement of an ACL: whether it *allows*, *denies*, or (for
  audit and alarm entries) only marks decisions worth recording; **who** it is
  about; **which rights** it covers; and **flags** saying whether it also applies
  to files created later below a directory (§2.2–§2.5). Windows calls it an ACE.
- **effective** — an entry that decides something about this file. An
  *inherit-only* entry applies only to future children.
- **right** — one thing a caller may be allowed to do, one bit (§2.4). A request
  asks for a set of rights.
- **the walk** — reading the entries top to bottom for one caller (§3.3).
- **share ceiling** — the most a caller may be granted on one share (§3.2).
- **the translation** — the one function pair, shared by both adapters, between
  a wire spelling and the stored ACL (§5).

What this RFC guarantees is listed as invariants in [§7](#7.%20Invariants).

---

## 1. Purpose

Two protocols that describe permissions differently, served from one store,
need one meaning for a stored ACL, or each adapter grows its own, and a second
meaning is where a hole hides. This RFC fixes that meaning once.

It adds no component with state or a lifecycle. The sequence of §3 runs inside
RFC 7's chokepoint, in the entity layer written once over every backend
([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)).
The functions of §4 are on RFC 7's `ACL` type. The translation of §5 is
stateless functions in the adapters' shared translation package
([RFC 20 §2.5](rfc-20-adapters.md#2.5%20Architecture%20and%20package%20layout)).

### 1.1 Non-goals

This RFC **MUST NOT** decide who the caller is (RFC 18), where and how often a
permission is checked ([RFC 7 §7.1](rfc-7-namespace-metadata.md#7.1%20One%20chokepoint),
[§7.2](rfc-7-namespace-metadata.md#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always),
[RFC 17 §4.6](rfc-17-vfs.md#4.6%20One%20chokepoint)), admission
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)), how
access events are delivered ([RFC 25 §4](rfc-25-observability.md#4.%20Event%20streams)),
or any wire encoding and status code (RFC 21, RFC 22).

## 2. The model

### 2.1 An ACL

An ACL is two ordered lists and their flags
([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)'s
`ACL`):

- the **access list** — allow and deny entries, which the walk reads;
- the **audit list** — audit and alarm entries, which never grant or deny and
  are read only to select access events (§3.6);
- each list's **flags** (§2.6).

Owner and group are not part of the ACL. They are fields of the File, which
the ACL refers to through the special principals `OWNER@` and `GROUP@` (§2.3).

### 2.2 Entry kinds

| Kind | List | Effect |
| --- | --- | --- |
| allow | access | grants the rights it covers that no earlier entry decided |
| deny | access | refuses the rights it covers that no earlier entry decided |
| audit | audit | selects an access event for a decision it matches (§3.6) |
| alarm | audit | as audit; there is no separate alarm channel |

No other kind is held. Object, callback (conditional), mandatory-label,
resource-attribute and scoped-policy entries, which security descriptors can
carry, are refused at the translation ([RFC 22 §11](rfc-22-smb.md#11.%20Security%20descriptors)).

> ponytail: an alarm entry is an audit entry. Neither protocol defines what an
> alarm does beyond "raise an alarm", and the access-event stream is the one
> channel the service has. Give alarms their own event type when
> [RFC 25 §4.1](rfc-25-observability.md#4.1%20Four%20streams), which defines no
> second consumer today, gains one that must tell them apart.

### 2.3 Principals

An entry names its principal by ID ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)),
and the principal's kind is part of the entry: a user entry never matches a
caller through a group of the same number, and a group entry never matches a
user. An entry names one of:

- **a user or a group** — matched when it is the caller's principal or one of
  its groups;
- **a special principal** — `OWNER@`, `GROUP@`, `EVERYONE@`, which match the
  file's current owner, members of the file's current group, and every caller;
- **a well-known principal** — matched as
  [RFC 18 §2.1](rfc-18-identity.md#2.1%20Kinds%20and%20well-known%20principals)'s
  table says, by a field of the caller and never by privilege: a privileged
  caller skips the walk, so a principal matched only by privilege would grant
  nothing anyone walks. A well-known principal with no row there matches no
  caller; §2.8 says what a set naming one does.

Matching reads the caller [RFC 18 §6.1](rfc-18-identity.md#6.1%20From%20Identity%20to%20caller)
resolved — its principal, groups, deny-only principals and `authenticated` and
`admin` flags — and nothing else. It never reads a UID, GID, SID or name: those
are spellings, resolved to principals before an entry is stored or a caller
arrives.

- **Deny-only principals match deny entries only.** A deny entry naming one of
  the caller's deny-only principals matches it; an allow entry naming one
  **MUST NOT**, and no share grant is read for one. Counted as a group, it would
  grant what the proof withheld.
- **An asserted identity is not authenticated**, so it matches no
  Authenticated Users entry (RFC 18 §2.1): a tree whose ACL grants only
  Authenticated Users is closed to every asserted-identity caller, which needs a
  named entry, its group's or everyone's instead. A named entry was written for
  that principal on a share that accepts asserted identities; Authenticated Users
  promises a proof the caller never gave.
- **A caller acting as the share's anonymous principal** matches `OWNER@` only
  on a file it owns, like any principal.

> decision: every squashed or anonymous caller of a share is one principal, so
> each owns what any of them created and holds the owner's implicit rights on it
> (§3.1 step 5). That is POSIX `all_squash`. Over SMB it is reached by a
> guest on a share that enables guest access
> ([RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in)), where every guest is the
> same anonymous principal, as every `all_squash` client is. Revisit if a
> deployment needs one squashed or guest caller kept from another's files.

`OWNER@` and `GROUP@` follow the file: after a `chown` they match the new owner.
An entry naming the owner's own principal does not; it keeps matching that
principal.

**Classes, for the mode.** §4 reads an ACL by three classes of principal:

- the **everyone-like** principals — `EVERYONE@`, Authenticated Users,
  BUILTIN\Users, NETWORK and ANONYMOUS LOGON — which match callers whatever
  their memberships, and are the *other* class;
- the **named** principals — users, groups, root and Administrators — which
  match only some callers;
- `OWNER@`, `GROUP@` and owner rights, which follow the file; SYSTEM, CREATOR
  OWNER and CREATOR GROUP match no caller.

### 2.4 Rights

A right is one bit of an entry's mask and of a request. The bit values are the
ones NFSv4 and Windows already share, so a mask crosses the translation
unchanged. On a directory three bits carry a second name.

| Right | On a directory | Bit |
| --- | --- | --- |
| read data | list | `0x00000001` |
| write data | add a file | `0x00000002` |
| append | add a subdirectory | `0x00000004` |
| read extended attributes | | `0x00000008` |
| write extended attributes | | `0x00000010` |
| execute | traverse | `0x00000020` |
| delete a child | | `0x00000040` |
| read attributes | | `0x00000080` |
| write attributes | | `0x00000100` |
| delete | | `0x00010000` |
| read the ACL | | `0x00020000` |
| write the ACL | | `0x00040000` |
| change the owner | | `0x00080000` |
| synchronize | | `0x00100000` |

These fourteen are the **entry rights**: the only bits a stored entry's mask
holds. A request may also carry two flags that are not entry rights:

- **system security** (`0x01000000`) — read or write the audit list (§3.5);
- **a maximal request** (`0x02000000`) — "grant me whatever I may have" (§3.4).

**Rights arrive mapped.** A protocol that carries generic rights maps them to
entry rights before the call and before an entry is stored; the mapping is
[RFC 22 §16.3](rfc-22-smb.md#16.3%20Namespace)'s, and NFS carries none. A
generic bit that reaches the walk is granted by no entry and no mode bit, so a
request holding one is refused; a set holding one is refused (§2.8).

A mask a client sets is reduced to entry rights before it is stored: every other
bit outside the fourteen — system security, the maximal flag, the retention bits
(`0x00000200`, `0x00000400`), reserved bits — is cleared and counted (§8).
Clearing a bit no request can be granted by narrows neither an allow nor a deny in
any way a request can observe.

What each operation asks for is §3.1's, [RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)'s
and [RFC 17](rfc-17-vfs.md)'s.

### 2.5 Entry flags

| Flag | Meaning |
| --- | --- |
| file inherit | copied to new files below a directory |
| directory inherit | copied to new subdirectories |
| no propagate | copied one level only |
| inherit only | governs children, not this file |
| inherited | this entry came from a parent |
| successful access | an audit entry selects allowed decisions |
| failed access | an audit entry selects refused decisions |

The protocols place the last three at different bits, so the translation maps
each flag by name (§5.1); the bits are [RFC 22 §11](rfc-22-smb.md#11.%20Security%20descriptors)'s and [RFC 21 §11.3](rfc-21-nfs.md#11.3%20Namespace)'s. An entry flag outside
the table is cleared and counted.

An entry is **effective** on a file when it is not inherit-only. Only effective
entries are walked, and only effective audit entries select events.

### 2.6 ACL flags, null and empty

Each list carries three flags, the same in both protocols (their bits are
RFC 22 §11's and RFC 21 §11.3's):

| Flag | Meaning |
| --- | --- |
| protected | entries are not inherited into this list from a parent (§4.4) |
| auto-inherited | the inherited entries were computed by the inheritance rules |
| defaulted | the list came from a default, not from a client |

An **empty** access list grants nothing to anyone except the owner's implicit
rights (§3.1 step 5), as both protocols specify. A **null** list — a set saying
it carries an access list and carrying none, which a Windows descriptor can —
means the opposite: everyone may do everything. It is stored as one entry,
allow `EVERYONE@` every entry right, which decides every request the same way,
and read back as that entry.

> ponytail: a null DACL is not a stored state. One allow-everyone entry decides
> every request exactly as a null DACL does, and keeps the walk free of a flag
> to check first. The cost is that a client reads back a one-entry DACL where it
> wrote a null one. Add the state when a client is shown to depend on reading a
> null DACL back.

### 2.7 Order

The access list is walked in **stored order**, and the stored order is the order
the client sent. The server **MUST NOT** reorder, merge or deduplicate entries
a client set: a later allow after a deny means something different from the
same allow before it, and the client chose. The one exception is RFC 22 §11's fold of
an owner or group pair into the one `OWNER@` or `GROUP@` entry it spells, which
changes no decision on the file or on any file created below it. A list need
not be canonical to be stored or walked.

The audit list has no order that matters: an audit entry never decides.

### 2.8 What a stored ACL may hold

A set **MUST** be refused whole, with `ErrInvalid`, and nothing stored, when:

- either list, counted as a security descriptor spells it ([RFC 22 §11](rfc-22-smb.md#11.%20Security%20descriptors)) — an
  effective, inheritable `OWNER@` or `GROUP@` entry counting two — holds more
  than 128 entries, or that spelling is larger than 64 KiB (both fixed values),
  so writing back what was read never exceeds the limit;
- an entry is of a kind §2.2 does not hold;
- a mask holds a generic bit (§2.4): cleared, a generic bit in a deny would
  narrow the deny;
- an entry's principal resolves to no principal (RFC 18's `ErrUnknownPrincipal`);
- a **deny** names a well-known principal with no row in
  [RFC 18 §2.1](rfc-18-identity.md#2.1%20Kinds%20and%20well-known%20principals)'s
  table: skipped by every walk, it would fail open. An allow naming one is
  stored and grants nothing. The principals in the table are stored in any
  entry: a deny naming SYSTEM is kept and never matches, and an inheritable
  deny naming CREATOR OWNER or CREATOR GROUP is kept, since inheritance makes it
  an effective deny for the creator.

Only a lookup answering that no principal holds the spelling is "resolves to no
principal". Any other lookup error fails the set with that error, never
`ErrInvalid` ([RFC 18 §5.4](rfc-18-identity.md#5.4%20Errors%20never%20become%20answers)),
so a client retries a set the store could not check.

A set **MUST NOT** be accepted with one entry left out, but for an entry naming
a deleted principal (§5.2). Leaving out an allow loses a grant the client meant;
leaving out a deny grants what the client forbade, and the client is told it
succeeded.

**A list the server builds** — the ACL a `chmod` merges (§4.3), the inherited
ACL of a create (§4.4) — **MUST** be within the same limit. One that would
exceed it fails the operation building it, `ErrInvalid` for a `chmod` and
`ErrNoSpace` for a create, and stores nothing. A list is never truncated to fit:
truncation can drop a deny. A read never truncates either: a reply that cannot
carry the list whole fails with the protocol's too-small status.

> decision: a SID no principal holds — a user of a domain the installation
> does not know — refuses the set, rather than being stored as a bare SID the
> walk would compare. An entry stores a principal ID and nothing else
> ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)); a bare SID
> would be a second, unmanaged identity space. The cost: copying a descriptor
> from another domain's server, as `robocopy /SEC` does, fails on its first
> foreign entry. Overturned by [RFC 18 §4.4](rfc-18-identity.md#4.4%20SIDs)
> minting a principal for a well-formed foreign SID, after which such an entry
> stores like any other.

## 3. Evaluation

### 3.1 One request, in order

`Authorize(want)` and `Granted` ([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface))
run this sequence, on the file the operation will act on
([RFC 7 §7.3](rfc-7-namespace-metadata.md#7.3%20The%20decision%20is%20against%20the%20file%2C%20not%20the%20name)),
after the service has admitted the call and resolved the caller
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)):

1. **The request.** `want` holds entry rights and, optionally, the maximal flag
   (§3.4) and the system-security right (§3.5). A request holding a generic bit
   is refused (§2.4).
2. **Limit: take the share ceiling** `C` (§3.2). A caller with no grant is
   refused `ErrAccess`. A caller whose `Access` is read because the share
   narrowed it — `ReadOnly` is the share's
   ([RFC 18 §6.1](rfc-18-identity.md#6.1%20From%20Identity%20to%20caller)) —
   asking any right outside the read row is refused `ErrReadOnly`, which the
   protocols answer as a read-only filesystem; one narrowed by its grant is
   refused `ErrAccess` at step 9.
3. **Collect: privilege.** A privileged caller, or a request through a
   privileged open (§3.5), is granted every entry right — except execute on a
   non-directory that no execute bit and no entry grants to anyone — and skips
   steps 5 and 6.
4. **System security**, if asked: granted only to a caller whose `admin` flag is
   set (§3.5); otherwise the request is refused whatever else it asked.
5. **The owner's implicit rights**, before any entry is read: the file's owner
   holds read and write the ACL — and, on a file with no stored ACL, read and
   write attributes — so no deny takes them away and an owner can always repair
   an ACL that locks it out, as a POSIX owner's `chmod` can. An effective
   **owner rights** entry withdraws them: the owner then gets only what the walk
   grants, owner-rights entries included. `OWNER@` entries still match the owner.
6. **The walk** over the stored access list, or over the ACL the mode implies
   when none is stored (§3.3, §4.1).
7. **The parent.** On a call that names the directory it resolved through
   (`via`), and only when `via` holds an entry naming the file, read in the
   decision's transaction: delete is granted when `via` grants delete a child,
   and read attributes when it grants list. In a directory whose mode has the
   sticky bit, **whether or not it has a stored ACL**, delete a child grants the
   removal only of an entry whose file the caller owns, in a directory the caller
   owns, or to a privileged caller. Adding, removing or renaming a name in a
   directory also asks traverse on that directory
   ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)).
8. **Remove: the flags** of [RFC 7 §7.8](rfc-7-namespace-metadata.md#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint)
   remove what they refuse, and on a non-directory the read-only marker removes
   write data and append
   ([RFC 7 §7.9](rfc-7-namespace-metadata.md#7.9%20The%20read-only%20marker%20refuses%20writes%20and%20deletes)).
   The marker's refusal of a delete is the delete's own, not a right removed
   here, since an open asking delete to rename the file is allowed.
9. **Cut to the ceiling**: the granted set `G` is what steps 3–8 granted, within
   `C`. `Granted` returns `G`. `Authorize` succeeds exactly when every right in
   `want` is in `G`, and is refused `ErrAccess` otherwise; a maximal request as
   §3.4 says.
10. **Select audit events** for the outcome (§3.6).

![One permission decision across three lanes: the adapter maps rights to entry rights, the service admits the call and builds the Caller, and the chokepoint takes the share ceiling, collects what privilege, ownership, the ACL walk and the parent grant, removes what the flags forbid and cuts the result to the ceiling. bob's write ends at his deny entry as ErrAccess, which the adapter answers STATUS_ACCESS_DENIED, and a call through an open uses the open's stored grant in place of the walk](img/rfc19-decision-flow.svg)

The order is part of the rule. The ceiling applies after privilege, so a
privileged caller on a read share is still read-only. The parent's delete is
added before the flags, so an immutable file refuses delete whatever its parent
grants. The owner's implicit rights are not removable by a deny, and the ceiling
still narrows them, so the owner of a file on a read share cannot rewrite its
ACL.

Four kinds of call take a different path through the sequence:

- **A call through an open** replaces steps 3–7 with the open's stored grant
  (§3.4): `G` is that grant, less what step 8's flags refuse, within the `C` of
  this call. Steps 2, 8, 9 and 10 run on every call, so a grant removed after
  the open, or a narrowing to read-only, takes effect at the next call.
- **An open that creates the file** is granted its request within `C`: the
  file did not exist, so its ACL and mode cannot refuse the creator, and
  `open(O_CREAT|O_WRONLY, 0444)` must succeed. The gate is the parent's add a
  file or add a subdirectory right, decided in the create's transaction
  ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)).
- **A call that only releases state** — a close, an unlock, a lock cancel, the
  acknowledgement of a lease, oplock or delegation break, the return of a grant,
  a logoff or tree disconnect — is not refused at step 2 when the caller's
  ceiling no longer admits it, and runs no walk.
- **A change of mode, owner, group or times** asks a right of this sequence, so
  it is cut by `C` before the ownership rules of
  [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)
  decide the value: a `chmod` asks write the ACL; a `chown` asks change the
  owner; a `chgrp` asks change the owner, or write the ACL when the caller owns
  the file; setting the times to a client-given value, or any other attribute,
  asks write attributes, and setting them to the server's time asks write data.
  So an owner on a read share cannot `chmod`, `chgrp` or `touch -d` its file.

> decision: release-only calls are exempt from the ceiling's refusal. Once a
> grant is revoked or a client rule narrows, a client that cannot close, unlock
> or acknowledge a break keeps its opens, locks and leases blocking every other
> client until their timeouts. The exemption holds only for calls that return no
> data and grant no state, so it reaches nothing the ceiling protects. Withdraw
> it for any call shown to return data or create state.

### 3.2 The share ceiling

A caller's `Access` is its share grant's level, already narrowed by admission
and carrying why it is read-only
([RFC 18 §6.1](rfc-18-identity.md#6.1%20From%20Identity%20to%20caller),
[§6.4](rfc-18-identity.md#6.4%20Share%20access%20from%20the%20grants)). The
**share ceiling** is the rights that level allows:

| Level | Ceiling |
| --- | --- |
| none (no grant) | nothing: the call is refused at step 2 |
| read | read data and list, execute and traverse, read attributes, read extended attributes, read the ACL, synchronize, system security |
| read-write | every entry right, system security |
| admin | every entry right, system security |

A file's ACL can narrow what the ceiling allows and never widen it: the ceiling
is an intersection, not an entry. It is never written into a file's ACL, stored
or shown; a share's root carries its own ACL like any directory (§4.5). Which
principal a grant is read for on a squashed share — the squashed one for a
claimed identity, the authenticated one for a proven identity — is
[RFC 18 §6.2](rfc-18-identity.md#6.2%20Squashing)'s.

> decision: `read-write` is every entry right, the ACL's included, unlike
> Windows' "Change" share permission, which withholds writing the ACL and the
> owner. NFS has no share-level split: `chmod`, `chgrp` and `cp -p` on one's own
> files write the ACL, and a ceiling without it would break every NFS client of
> a read-write share. Overturned when an operator needs a share whose users
> write data and may not re-permission it; that is a fifth level, not a change
> to this one.

### 3.3 The walk

The server reads the entries top to bottom and keeps, for each right asked for,
the first answer it meets. An entry that does not name the caller (§2.3), or is
not effective on this file, is skipped. An entry that names the caller answers
yes (allow) or no (deny) for each right it covers that is still unanswered, and
changes nothing that already has an answer. A right still unanswered at the end
is refused. So a deny above an allow wins, and an allow above a deny wins.
Entries are walked in stored order whether or not the list is canonical.

*Example.* bob asks for write data and read the ACL. Entry 1, `deny bob write
data`, answers write data: no. Entry 3, `allow builders modify`, answers read the
ACL: yes, and leaves write data alone. The request is refused.

`Granted` runs the same walk over every entry right without stopping early: an
allow adds the rights it covers that no earlier entry denied, a deny removes the
rights it covers that no earlier entry granted.

- **An entry naming a principal since deleted** matches no caller: no identity
  resolves to a deleted principal, and the ID is never reissued
  ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)), so no later
  account inherits the entry, allow or deny.
- **Owner rights and `OWNER@`** are both walked, in order (step 5).
- **The ACL a mode implies** is walked like any stored one. A file with no
  stored ACL has no second check path.

| ACL, in stored order | Caller | Asked | Answer |
| --- | --- | --- | --- |
| allow everyone read data | owner | read and write the ACL | granted (implicit) |
| deny owner write ACL; allow everyone read data | owner | write the ACL | granted (implicit, not removable by a deny) |
| allow owner rights read data | owner | write the ACL | refused |
| allow everyone read and write data; deny alice write data | alice | maximal | read and write data (stored order: the allow came first) |
| deny alice write data; allow everyone read and write data | alice | maximal | read data |
| no entries | owner | maximal | read and write the ACL |
| no entries | bob | maximal | nothing |

> decision: the ordered allow and deny walk, with the owner's implicit rights and
> owner rights as their one exception, is the evaluation. It is what the clients
> of every served protocol assume for a list of allow and deny entries, canonical
> or not, since each walks in stored order, and it lives behind
> `Files.Authorize` and `Files.Granted` only, so a change to it touches no caller.
> It does not model object or conditional entries, well-known principals beyond
> RFC 18's table, or privileges beyond one flag. Overturned by a case of the
> table above, or of §9, that a reference server of a served protocol answers
> differently.

### 3.4 Maximal access and an open's grant

`Granted` answers the whole granted set, for an access query, a maximal-access
query, and an open asking for whatever it may have
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)): the owner's implicit rights,
joined with what the walk allowed (for a privileged caller every right but the
execute step 3 withholds), less what the flags refuse, within the ceiling.
`Authorize(want)` **MUST** succeed exactly when `want` is a subset of `Granted`.

A **maximal request** — an open asking "whatever I may have", such as SMB's
`MAXIMUM_ALLOWED` — is granted `G`, and **MUST** be refused `ErrAccess` when `G`
is empty or when a right it also asked for by name is outside `G`, as Windows'
access check decides.

**What an open holds.** The grant stored with an open
([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)) is its mapped request — or `G`,
for a maximal request — computed in the evaluation that admitted it. A
privileged open's grant is likewise its mapped request (every entry right within
`C`, for a maximal one): privilege lifts the check on what was asked, and never
widens what was asked. A request through any open is refused a right outside
that grant — a write through a handle opened for read, a DACL set through a
backup handle opened to read — and a delete on close is decided by the delete in
the stored grant.

An access query answers `G` with the privilege the caller holds, since the
operation that follows is authorised with it. A maximal-access answer on an
open, such as SMB's `MaximalAccess` and `MxAc`, is computed without that open's
privilege (§3.5).

### 3.5 Privilege and the system-security right

Which callers are privileged is
[RFC 18 §6.5](rfc-18-identity.md#6.5%20What%20the%20proof%20proved%3A%20authenticated%2C%20admin%2C%20privileged)'s.
This section says what privilege changes in a decision.

- **Access.** A privileged caller is granted every entry right without walking
  the mode or ACL, so an unsquashed superuser reads and restores what the file's
  own permissions would refuse, as on a POSIX server — except **execute** on a
  non-directory, granted only when some execute bit of `Mode` is set or an entry
  grants it to someone, as `CAP_DAC_OVERRIDE` does. The flags still refuse it
  (step 8), and the ceiling still cuts it (step 9).
- **Privilege may be scoped to an open.** An open the service marks privileged
  makes its granted access and **every** later request through it privileged — a
  set of the owner to another principal, a write of the owner, group and ACL, a
  rename — and no other open of the same principal. Those requests reach the
  chokepoint with the open they came through (`SetAttrs`, `SetACL`, `Rename`,
  `Unlink` and the xattr calls take an open reference,
  [RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)), so the open's
  grant and privilege decide them, never a fresh unprivileged check. An open asks
  for it as a backup or restore intent (`OpenRequest.Backup`,
  [RFC 17 §3.0](rfc-17-vfs.md#3.0%20Value%20types)), and the service marks it
  privileged only for a caller RFC 18 makes privileged; which protocol request
  carries that intent is the adapter's ([RFC 22 §11](rfc-22-smb.md#11.%20Security%20descriptors)).
  A maximal-access answer is computed without an open's privilege, even on that
  open.
- What privilege decides beyond access — setting the owner or group to another
  principal, setgid, the immutable and append-only flags — is
  [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)'s.

**The system-security right** — reading or writing the audit list — is granted
at step 4 only to a caller whose `admin` flag is set, and never by an entry, by
ownership or by privilege alone. It is in every ceiling but none's (§3.2), so it
passes the cut like any right; that is harmless, since step 4 is the only place
it is granted. A maximal request never grants it: it is in `G` only when asked
for explicitly, as on Windows. Reading the audit list needs the right alone.
**Writing** it — a set naming the audit list, or a set of the whole ACL carrying
audit entries — needs the right and also write the ACL in the `C` of the call
that writes. So under an admission narrowed to read-only an administrator may
read the audit list, and a set of it is refused. `C` is taken per call, so an
open made before the admission narrowed cannot write the audit list after it.
The refusal each protocol answers is RFC 21's and RFC 22's (RFC 21 §11.3, RFC 22 §11).

### 3.6 Audit entries select events

Two switches decide whether an access is recorded. The **share's audit policy**
is the operator's: off records nothing; on records by its own rule — every
decision, denials only, or writes and namespace changes — and, if set to honour
file entries, also what the file's **audit entries** select
([RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)).
The chokepoint ([RFC 7 §7.6](rfc-7-namespace-metadata.md#7.6%20Decisions%20are%20observable))
emits nothing and does not read the policy: it returns, with the decision,
whether an audit entry selected it, and the filesystem service applies the
policy and emits the event ([RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)).

An effective audit or alarm entry selects a decision when:

- it names the caller, matched as the walk matches (§2.3);
- its mask holds at least one right the request asked for — for a maximal
  request, the rights it asked for are, when it was granted, `G` and every right
  it asked by name; when it was refused, every right it asked by name, or every
  entry right when it asked none;
- it carries the successful-access flag and the decision granted, or the
  failed-access flag and the decision refused.

One decision produces at most one event, however many entries select it. A
privileged caller's decisions are selected like anyone's. A query that asks no
right — an access query, a maximal-access query — selects nothing. Audit entries
are inherited like access entries (§4.4), so an entry on a share's root reaches
every file created below it after it was set. With the policy off, no entry
selects anything and no event is built, as on Windows, where an audit entry has
effect only while object-access auditing is on.

A cached decision is keyed as
[RFC 7 §7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read)
says. The outcome and the audit selection both read the request — one caller
granted read and write, under a successful-read entry, is selected for a read and
not for a write — so the key holds the request's mapped rights and flags. The
audit policy is applied after the decision and is in no key.

### 3.7 Listings hide what the caller cannot read

A share whose policy asks for access-based enumeration (the share setting of
[RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings),
spelled on the wire by RFC 22) has every listing the service returns on it filter
each entry: an entry is listed only when `Granted` on its file, through the
listed directory, holds read data, read attributes, read extended attributes or
read the ACL. Each entry's answer is an ordinary `Granted`, run at the chokepoint
and keyed as RFC 7 §7.5 says; no adapter decides it.

> decision: access-based enumeration filters SMB listings only; an NFS listing is
> never filtered. A hidden name still exists, so an NFS client's create of it
> fails with "exists" and `rm -r` of its parent with "not empty", with nothing the
> client was shown explaining either, and POSIX tools assume a listing is
> complete. Windows clients expect the filter, and home-directory shares rely on
> it to hide other users' folder names. Overturn when an operator needs the same
> hiding over NFS and accepts those failures.

### 3.8 Locks outside an open

A byte-range lock taken outside any open — NFSv3's lock manager, which has no
open whose grant could gate it — needs read data, or ownership of the file. A
write lock asks no more than a read lock. A lock taken through an open is gated
by that open's grant (§3.4).

Ungated, any caller could lock another user's 0600 file and hang its owner's
locking callers; requiring write for a write lock would break the common
lock-then-write on a descriptor opened for read and later reopened.

## 4. Mode and ACL

![The mode and the ACL derive from each other: the ACL is implied by the mode when none is stored, the mode is re-derived on every ACL set, and chmod merges into a stored ACL. The chmod 750 example narrows the named allows by splitting each into an unchanged inherit-only copy and a limited effective copy, leaves bob's deny untouched and appends one OWNER@ allow for write at the end](img/rfc19-mode-acl.svg)

RFC 7 requires the two to agree after every transaction
([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)).
This section defines how: the ACL a mode implies, the mode an ACL implies, the
`chmod` merge and inheritance. `ACL.Mode` and `ACL.WithMode` take the file's
current owner `o`: ownership is a field of the File, not of the ACL (§2.1), and
the same entries imply different modes on files with different owners —
`allow alice READ_DATA` is 0400 on a file alice owns and 0040 on one bob owns.

```go
func ACLFromMode(mode uint32, dir bool) ACL                         // §4.1
func (a ACL) Mode(owner Principal) uint32                           // §4.2: the nine bits a implies on a file owner owns
func (a ACL) WithMode(mode uint32, owner Principal, dir bool) ACL   // §4.3: the chmod merge
```

### 4.1 The ACL a mode implies

A mode like 0750 means three things: the owner gets rwx, the file's group gets
r-x, everyone else gets nothing. The server spells that as up to five entries,
in order: (owner deny), owner allow, (group deny), group allow, everyone allow.
The two deny entries appear only when a later class has a bit an earlier one
lacks, as in 0604, where "other" may read and the group may not.

**What a mode bit grants**, `R(bits)`. A protocol's read, write and execute
requests carry rights beyond the data right, and a check needs every right it
asks for, so a mode bit grants the whole of its set to whoever it applies to,
owner or not. **Any** bit grants the minor rights every such set carries —
synchronize, read the ACL, read attributes and read extended attributes. Beyond
them a read bit grants read data; a write bit grants write data, append, write
extended attributes and write attributes, and on a directory add a file, add a
subdirectory and delete a child; an execute bit grants execute. A group member's
request for the read and write sets on a 0664 file is therefore granted, and so
is a request for the write set alone on a 0220 file or the execute set on a 0110
one. Write attributes granted by a mode bit is granted at open, so through that
open a non-owner may set the times, client-given ones included
([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)),
while a set of the stored markers, the mode or the owner by a non-owner is refused
`ErrPerm` at the set ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)).

`ACLFromMode(m)` is the ACL a file with no stored ACL is evaluated against and
shown as. With `u`, `g`, `o` the owner, group and other bits of `m`, it is, in
this order, with every entry whose mask is empty left out:

1. deny `OWNER@` `R(g|o)` minus `R(u)`;
2. allow `OWNER@` `R(u)`;
3. deny `GROUP@` `R(o)` minus `R(g)`;
4. allow `GROUP@` `R(g)`;
5. allow `EVERYONE@` `R(o)`.

None is inheritable, and the list carries the defaulted flag. This is POSIX's
class rule: an owner gets the owner bits and nothing a group or other class adds;
a group member who is not the owner gets the group bits; anyone else the other
bits. For every mode whose classes only narrow — 0755, 0750, 0644, 0640, 0600 —
no deny entry is produced.

> decision: the mode-implied ACL has a deny entry only where a mode cannot be
> expressed without one, such as 0604. An allow-only form would give the group
> of a 0604 file the other class's read, which POSIX refuses; an always-deny
> form shows every Windows user a "Deny" on every ordinary file. Overturned if a
> Windows tool is shown to mishandle the deny entries of the rare modes that
> need them.

**What a client is shown is what is evaluated.** A security descriptor or `acl`
read from a file with no stored ACL **MUST** be `ACLFromMode` of its mode,
spelled by §5. The server **MUST NOT** show a different default (owner and
SYSTEM full control, say) while evaluating the mode: a backup tool copies what
it is shown to another server as the file's real permissions.

### 4.2 The mode an ACL implies

Going the other way, the server works out the mode an ACL implies by asking what
imaginary callers would get. `a.Mode(o)` gives the nine permission bits an ACL
implies on a file owned by `o`; setuid, setgid and the sticky bit are not in an
ACL and are kept from the stored `Mode`. Each class is the bits of what the walk
(§3.3) would grant, mapped back — read from read data, write from write data,
execute from execute:

- **owner class**: a caller who is `o`, in no group, matched by `OWNER@`, owner
  rights, `EVERYONE@`, NETWORK and entries naming `o`;
- **group class**: the union, over `GROUP@` and every **named** entry (§2.3)
  that does not name `o`, of what a caller matched by that principal, `EVERYONE@`
  and NETWORK alone is granted: the mask POSIX ACLs show in the group bits;
- **other class**: the union of what three strangers are granted, each matched
  by `EVERYONE@` and NETWORK — an authenticated one also by Authenticated Users
  and BUILTIN\Users, an anonymous one also by ANONYMOUS LOGON, and one with an
  asserted identity by nothing more. So the other bits never under-report what
  every caller of one kind holds.

Only effective entries count, and the owner's implicit rights do not: they are
not mode bits.

The group class is the union over named principals, not `GROUP@` alone, so that
the merge — which limits every named allow to the new group bits — is an identity
on the ACL's own mode. A `cp -p` or `chmod` that sets the mode it just read must
grant afterwards exactly what was granted before.

Two identities **MUST** hold, and are checked (§9):

- `ACLFromMode(m, d).Mode(o) == m` for every nine-bit `m`, both file types `d`
  and every owner `o`;
- for every ACL `a` and owner `o`, `a.WithMode(a.Mode(o), o)` grants every
  caller of a file owned by `o` exactly what `a` grants — for every `a` whose
  allows hold write data wherever they hold another right the write bit adds
  (§10, item 5).

### 4.3 Setting mode and ACL

`chmod` on a file with a stored ACL never raises an entry and never moves one.

- **A `chmod` of a file with a stored ACL** is the merge `WithMode(m, o)`:
  - **It only limits.** Every effective allow is limited to its class's new
    bits: `OWNER@` allows to the owner bits; allows naming an everyone-like
    principal (§2.3) to the other bits; when the ACL has an effective named allow
    not naming `o`, every named allow **and** every `GROUP@` allow to the group
    bits — the group bits are the mask, as a POSIX `chmod` sets the mask and
    leaves the owning group's entry alone; otherwise `GROUP@` allows to the group
    bits. A limit removes only the rights of `R` (§4.1) a missing bit withholds,
    never the minor rights every bit grants, and keeps every other right of the
    entry.
  - **It adds only what a new bit grants.** For each class, the bits the new mode
    sets that the limited ACL does not already imply for it are granted by one new
    allow — `OWNER@`, `GROUP@` (only when no named allow holds the group class)
    and `EVERYONE@`, each with `R` of those bits — **appended at the end** of the
    access list, after every inherited entry, so it can override no deny. An
    entry it would add with an empty mask is left out. A `chmod` to the file's own
    mode therefore adds nothing.
  - An inherit-only entry is left untouched: it governs children. An entry both
    effective and inheritable is split in place into an inherit-only copy,
    unchanged, and an effective copy, limited as above.
  - Deny, audit and alarm entries are left untouched.
  - `Mode` becomes the merged ACL's `Mode(o)`, which equals the requested bits
    unless a deny withholds some or named entries grant less than the requested
    group bits; the stored `Mode` shows what is granted, never more. A merge that
    would exceed §2.8's limit fails `ErrInvalid` and changes nothing.
- **A `chmod` of a file with no stored ACL** changes `Mode` and stores no ACL.
- **Setting an ACL** stores it and stores `Mode` as its `Mode(o)` for the owner
  the file has after the set, with the special bits kept, in one transaction.
- **A request that sets the ACL and the mode together** stores the ACL, and takes
  from the mode only setuid, setgid and sticky.
- **A `chown` or `chgrp`** changes no entry. `OWNER@` and `GROUP@` follow it.
- The right each of these asks is §3.1's.

### 4.4 Inheritance

When a file or directory is created, it copies the parent's entries that are
marked to pass down, in the create's transaction, and takes the `Mode` the new
list implies. A parent with no inheritable entry gives the new file no stored
ACL, and its `Mode` governs. The inherited list is computed from the parent's
lists, each entry in stored order, as [RFC 8881 §6.4.3](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4.3)
and MS-DTYP 2.5.3.4 compute it:

| Parent entry's flags | New file gets | New directory gets |
| --- | --- | --- |
| neither inherit flag | nothing | nothing |
| file inherit | an effective copy, no inherit flags | an inherit-only copy keeping file inherit, unless no propagate |
| directory inherit | nothing | an effective copy keeping directory inherit, unless no propagate |
| both | an effective copy, no inherit flags | an effective copy keeping both, unless no propagate |
| no propagate, with either | as above, no inherit flags | an effective copy with no inherit flags, where a directory gets one |

- **Creator entries.** A copy of an entry naming CREATOR OWNER or CREATOR GROUP
  names `OWNER@` or `GROUP@`, so it follows the new file's own owner or group. A
  new directory's copy of such an entry with both inherit flags is one `OWNER@`
  (or `GROUP@`) entry, effective and inheritable, never an effective entry beside
  an inherit-only CREATOR OWNER copy: `OWNER@` names whoever owns each file, so a
  file bob later creates below gets an entry for bob, as the CREATOR OWNER copy
  would give it. RFC 22 spells the one entry as two on the wire ([RFC 22 §11](rfc-22-smb.md#11.%20Security%20descriptors)).
- **The inherited flag.** A copy carries the inherited flag when the parent's
  list carries the auto-inherited flag, and the new list carries the
  auto-inherited flag when the parent's does. A parent's protected flag does not
  pass to children; a list with the protected flag set inherits nothing.
- **The create's mode is an *and*.** It limits the inherited effective entries
  as a `chmod` merge would limit them, and never adds an entry or a right the
  inherited ACL did not hold. A umask the create carries apart from the mode
  ([RFC 21 §11.3](rfc-21-nfs.md#11.3%20Namespace)) is ignored when the parent has
  inheritable entries and applied to the mode when it has none, so an inherited
  "Finance: modify" is not cut to read and execute by a umask the client applied
  for a parent without them.
- **A create that carries an ACL** sets the new file's access list in place of
  the inherited one. The audit list stays the inherited one unless the caller
  holds the system-security right (§3.5) and the create carries audit entries.
- **A limit.** An inherited list that would exceed §2.8's limit fails the create
  `ErrNoSpace`; it is never truncated.

**Never to descendants.** A change to a directory's ACL changes no other file.
Propagating it down a tree is the client's: Windows tools walk the tree and set
each file, and NFSv4 has no propagation. A server-side walk of an unbounded
subtree inside one call would hold the directory's transaction for as long as
the tree is large.

> ponytail: no server-side propagation. Clients that propagate already walk the
> tree themselves, one set per file. Add an asynchronous, resumable propagation
> job when an operator needs to change the permissions of a tree too large for a
> client to walk.

### 4.5 A share's root

A share's root is a directory like any other: its owner, group and mode, and any
ACL, are set when the share is created
([RFC 23 §3.2](rfc-23-control-plane.md#3.2%20A%20share)) and changed by the
same calls as any directory's. No share grant is written into it (§3.2), so a
share whose users create at its top needs a root whose owner, group, mode or ACL
lets them.

## 5. The shared translation

### 5.1 One translation, two spellings

The server stores one ACL per file. Each protocol spells it its own way: Windows
names people by SID and packs flags one way; NFSv4 names them by `user@domain`
and packs some flags differently. Both adapters use **one translation**, a set
of stateless functions over neutral types that both adapters import
([RFC 17 §4.2](rfc-17-vfs.md#4.2%20Translation%20stays%20in%20adapters),
[RFC 20 §2.3](rfc-20-adapters.md#2.3%20What%20an%20adapter%20may%20import)), so
the two spellings cannot drift apart:

```go
// Names resolves principals for the translation: RFC 18's mapping, reached
// through vfs.Service's Spell and Find and bound to one call's caller, so a who
// is spelled for that call's proof kind (RFC 18 §4.5). The translation does no
// lookup of its own, and passes every error the name source returns through.
type Names interface {
	SID(p metadata.Principal) (SID, error)            // every principal an entry can name has one
	PrincipalBySID(s SID) (metadata.Principal, error) // ErrUnknownPrincipal for a SID no principal holds
	Who(p metadata.Principal) (string, error)         // NFSv4 who: OWNER@…, the owner string for the proof kind, or SID@domain
	PrincipalByWho(who string, group bool) (metadata.Principal, error)
}

type Attr uint8 // the NFSv4 attribute: acl, dacl or sacl (RFC 21 §11.3)

// NFSv4ACE is one nfsace4, as the NFS adapter's codec decodes it.
type NFSv4ACE struct {
	Type, Flags, Mask uint32
	Who               string
}

// Normalised is what a set had cleared or replaced to fit the model; the adapter counts it (§8).
type Normalised struct{ MaskBits, EntryFlags, DeletedPrincipals, NullDACL int }

// owner, group: the file's before the set (RFC 22 §11). a holds only the lists the caller may read (§3.5).
// p: the parts the request's security-information bits name; for a CREATE's descriptor, the parts it holds (RFC 22 §11).
// flags: the list's aclflag4 for dacl and sacl; zero for acl, which carries none (RFC 21 §11.3).
func DescriptorFromACL(a metadata.ACL, owner, group metadata.Principal, p metadata.ACLParts, n Names) ([]byte, error)
func ACLFromDescriptor(sd []byte, owner, group metadata.Principal, p metadata.ACLParts, n Names) (metadata.ACLSet, Normalised, error)
func NFSv4FromACL(a metadata.ACL, attr Attr, n Names) (v []NFSv4ACE, flags uint32, sidWhos int, err error)
func ACLFromNFSv4(v []NFSv4ACE, flags uint32, attr Attr, owner, group metadata.Principal, n Names) (metadata.ACLSet, Normalised, error)
```

`SID` is RFC 18's. `ACLParts` names which parts a request reads or sets — owner,
group, access list, audit list — and `ACLSet` is a set by those parts: owner,
group, each list's entries and flags, and the request to recompute inheritance.
The store applies a set by its parts in one transaction
([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface)'s `SetACL`), so a
part the set does not name is left as stored. That covers the audit list a
caller without the system-security right cannot read, and an adapter never reads
a part to write it back.

The translation **MUST**:

- change principals' spellings, flag bit positions and list framing, and
  nothing else: the kinds, the rights, the order and the flags' meanings cross
  unchanged; each flag is mapped by name, since a mask copied byte for byte turns
  NFSv4's inherited flag into Windows' failed-access flag;
- refuse, with `ErrInvalid`, anything §2.8 refuses, rather than store less than
  it was given;
- round-trip: for every ACL `a` the model holds, reading `a` through one protocol
  and writing back what was read stores `a`, and so does the same through the
  other protocol, for a caller allowed to see both lists.

The translation decides nothing about access: whether the caller may read or
write a part, and which lists it is handed, are the chokepoint's, asked with the
rights RFC 22 §11 and RFC 21 §11.3 name.

### 5.2 Principals on the wire

[RFC 18](rfc-18-identity.md) **MUST** give every principal an entry can name a
SID and an NFSv4 `who`, so that no entry is ever unprintable in either protocol.
A principal with no file spelling — a machine or an API-only account
([RFC 18 §2.4](rfc-18-identity.md#2.4%20Principals%20with%20no%20file%20spelling))
— holds no SID and no name an owner string resolves, so no entry can name one.
How each protocol spells each principal is
[RFC 21 §11.3](rfc-21-nfs.md#11.3%20Namespace)'s and
[RFC 22 §16.3](rfc-22-smb.md#16.3%20Namespace)'s. A principal a protocol has no
name for is spelled some other way that protocol carries — NFSv4 by its SID — and
never left out of a read: leaving it out makes every read-modify-write through
that protocol a silent delete of the entry, a deny included, which is the
failure of [Start here](#Start%20here).

**A deleted principal** has no spelling left
([RFC 18 §4.4](rfc-18-identity.md#4.4%20SIDs) shows it as the null SID
`S-1-0-0`); its entries match no caller (§3.3). They are shown with that
spelling, and on a set an entry naming the null SID, or NFSv4's spelling of it,
is left out and counted. This is the one entry a set may leave out (§2.8, Z2,
Z8): it matched no caller before the set and cannot match one after it, so no
request is answered differently.

> decision: an entry naming a deleted principal is left out of a set, the one
> exception to §2.8, because it matched no caller before and cannot after.
> Overturned the day a deleted principal can be restored under its ID: the
> dropped entries would then be grants and denies the restore should have
> revived.

## 6. A worked example across protocols

alice, bob and the build host's principal `build` are users of the `example.com`
domain; `build` and bob are members of `builders`. The `builds` share is
exported over NFS and SMB. alice's, bob's and `builders`' grant on `builds` is
`read-write`. `release` is a directory alice owns, group `builders`, with no
stored ACL and mode 0775.

**1. alice opens the Security tab from alice-pc.** Her implicit right as owner
answers the query (§3.1 step 5). She is shown `ACLFromMode(0775)` (§4.1),
spelled with SIDs (RFC 22 §11):

| # | Kind | Principal shown | Rights | Flags |
| --- | --- | --- | --- | --- |
| 1 | allow | alice's SID (stored `OWNER@`) | `R(rwx)` | none |
| 2 | allow | `builders`' SID (stored `GROUP@`) | `R(rwx)` | none |
| 3 | allow | Everyone | `R(r-x)` | none |

**2. alice sets a new DACL**, which Explorer writes in canonical order with the
protected and auto-inherited flags:

| # | Kind | Principal | Mask | Flags |
| --- | --- | --- | --- | --- |
| 1 | deny | bob | `0x116`: write data, append, write extended attributes, write attributes | `OI CI` |
| 2 | allow | alice | `0x1F01FF`: full control | `OI CI` |
| 3 | allow | `builders` | `0x1301BF`: modify | `OI CI` |

The set asks write the ACL, which alice holds implicitly. The entries are stored
in this order (RFC 22 §11); entry 2 names the owner's SID with inherit flags, so it
stays a named entry for alice (RFC 22 §11). `Mode(alice)` (§4.2) is 0770: owner `rwx`
from entry 2, group `rwx` from `builders` (bob's only entry is a deny), other
nothing.

**3. build01 reads it.** `nfs4_getfacl` on a Kerberos mount, as `build`, which
reads the ACL through `builders`' modify (§3.3) and holds no system-security
right, so no audit entry is returned. The translation spells (§5.2, RFC 21 §11.3):

```text
D:fd:bob@example.com:waNT
A:fd:alice@example.com:rwaDdxtTnNcCoy
A:fdg:builders@example.com:rwadxtTnNcy
```

`ls -l` shows `drwxrwx---`.

**4. A new file inherits.** build01 creates `release/notes.txt` with mode 0664.
The file gets the three entries as effective copies with no inherit flags and,
since the parent's list is auto-inherited, the inherited flag (§4.4). The create
mode limits the named allows to its group bits, `rw-`, and adds nothing. No entry
names `build`, and the ACL holds no `OWNER@` or `EVERYONE@` entry, so `Mode(build)`
is 0060: `build` reaches its own file only as a member of `builders`. A parent
that should give each creator owner bits carries an inheritable CREATOR OWNER
entry (§4.4).

**5. bob is refused over both protocols.** bob opens `release/notes.txt` from
Windows asking `GENERIC_WRITE`, which the SMB adapter maps to `0x120116`
([RFC 22 §16.3](rfc-22-smb.md#16.3%20Namespace)). Entry 1 answers write data,
append, write extended attributes and write attributes first: refused. Entry 3
answers read the ACL and synchronize. The open is refused,
`STATUS_ACCESS_DENIED`. Over NFSv4 the `OPEN` for write is refused,
`NFS4ERR_ACCESS`, and `ACCESS` asking `MODIFY` and `EXTEND` reports neither. A
read succeeds over both.

**6. build01 writes, and re-permissions its own file.** `build` writes
`notes.txt` through entry 3. `nfs4_setfacl -a` on `notes.txt` asks write the
ACL, which `build` holds implicitly as owner (§3.1 step 5); the tool reads three
entries, appends one and writes four back, bob's deny first and intact. The same
tool on `release`, which alice owns, is refused.

**7. alice runs `chmod 750 release` from a Linux box** (§4.3). The named allows
for alice and `builders` are each split into an inherit-only copy, unchanged, and
an effective copy limited to the new group bits, `r-x`. bob's deny is untouched.
The limited ACL implies `r-x` for the owner, and the new owner bits set write, so
one allow `OWNER@` `R(-w-)` is appended at the end. No `GROUP@` allow is added:
the `builders` allow holds the group class. `Mode(alice)` is 0750. `builders` can
no longer add files to `release` itself, and every file created below it still
inherits modify from the unchanged inherit-only copy.

## 7. Invariants

| # | Invariant |
| --- | --- |
| Z1 | A stored entry names its principal by ID, holds only the fourteen entry rights, and is one of four kinds. |
| Z2 | A set either stores every entry it carried, in its order — entries naming deleted principals aside (§5.2) — or stores nothing. |
| Z3 | The same request by the same caller of the same file is granted the same set, whatever protocol carried it. |
| Z4 | `Authorize(want)` succeeds exactly when `want` is within `Granted`, and `Granted` is within the share ceiling. |
| Z5 | No file ACL widens a share ceiling, and no share ceiling is written into a file's ACL, stored or shown. |
| Z6 | The ACL a client is shown is the ACL the walk reads, `ACLFromMode` of the mode where none is stored. |
| Z7 | `ACLFromMode(m, d).Mode(o) == m`; `a.WithMode(a.Mode(o), o, d)` grants what `a` grants on a file owned by `o`, for every `a` §4.2 names; and no `chmod` raises an existing allow or places a new one before any other entry. |
| Z8 | Reading an ACL through either protocol and writing back what was read stores the same ACL, less entries naming deleted principals (§5.2); over SMB, a named allow with no inherit flags for the file's current owner or group comes back as `OWNER@` or `GROUP@` (RFC 22 §11). |
| Z9 | Every principal an entry can name has a SID and an NFSv4 `who`; no entry is left out of a read for want of a spelling. |
| Z10 | Audit and alarm entries never change a decision; the system-security right is granted only to a caller with `admin` set. |
| Z11 | A change to one file's ACL changes no other file's. |
| Z12 | No list is truncated to fit the size limit, on a set, a `chmod`, a create or a read. |
| Z13 | A sticky directory refuses a non-owner's removal of another's entry whatever its ACL. |

## 8. Observability

Metric names are shown without the `dittofs_` prefix the exporter adds
([RFC 25 §2.1](rfc-25-observability.md#2.1%20Names)). Decision counts and latency
are the filesystem service's, which counts what the chokepoint returns; events
dropped are RFC 25's `events_dropped_total`. Access events are counted by RFC 25's
`events_records_total{stream="access"}`; a record's `selected_by` says whether
the policy or an entry selected it.

| Answers | Metric | Type |
| --- | --- | --- |
| ACL sets refused, and why (`kind`, `principal`, `limit`, `deny_unmatched`, `generic`) | `acl_set_refused_total{protocol, reason}` | counter |
| what a set had cleared or replaced to fit the model (§2.4, §2.5, §2.6, §5.2) | `acl_set_normalised_total{protocol, kind}`: `mask_bit`, `entry_flag`, `deleted_principal`, `null_dacl` | counter |
| reads spelled with a SID fallback `who` (§5.2) | `acl_who_sid_fallback_total` | counter |

> ponytail: no counter of refusals made by the share ceiling rather than the file.
> Telling the two apart costs a second, ceiling-free evaluation of every refusal.
> Add `acl_ceiling_refusals_total` when operators report "the file allows it but
> I'm refused" confusion they cannot diagnose from the share grant.

`acl_set_normalised_total` and `acl_who_sid_fallback_total` are not errors; a
sudden rise names a client sending what the model does not hold, or principals
RFC 18 cannot name. No metric carries a share or principal label
([RFC 7 §11.2](rfc-7-namespace-metadata.md#11.2%20Observability)). The adapter
increments the set and spelling counters from what the translation returns
(§5.1). This RFC opens no span ([RFC 25 §6.1](rfc-25-observability.md#6.1%20Spans)),
and a refusal is expected and logs at `Debug`.

## 9. Conformance

Tiers are the harness's ([index](rfc-index.md#Test%20tiers)). Each row runs
through both adapters on the composed node unless it names one, so the check
sits at the consumer of the translation, not beside it. `ACLFromDescriptor`
parses client bytes, so it is a decoder under
[RFC 20 §5.4](rfc-20-adapters.md#5.4%20How%20codecs%20are%20tested): golden
vectors and a fuzz target. `ACLFromNFSv4` is fuzzed over generated entries.
Neither panics, and a refused input returns `ErrInvalid`, or the lookup's error,
and nothing else. A row that names a Windows server is a wire test of
[RFC 20 §8](rfc-20-adapters.md#8.%20Wire%20tests), run against its reference
server.

| § | Check |
| --- | --- |
| §3.3 walk and maximal access | Run each row of §3.3's table through `Authorize` and `Granted`, and the same ACLs on a reference server of each served protocol: assert every answer equals the table's and the reference servers'. For each ACL and caller, assert `Authorize(want)` succeeds exactly when `want` is within `Granted`. A build without implicit owner rights fails the first two rows; one that stops the maximal walk at the first allow fails the fourth. |
| §2.3 well-known matching | Store an ACL granting Administrators full and Authenticated Users read, with no other entry. As the share's `admin`, authenticated and not privileged: assert full access through the walk; as another authenticated user: read only; as that user with an asserted identity: nothing, and with a named entry for its principal added: what the entry grants. Store a deny naming an unmodelled well-known principal: assert `ErrInvalid`; a deny for NETWORK: assert it refuses every caller. A squashed caller matches `EVERYONE@` and ANONYMOUS LOGON, not Authenticated Users, and `OWNER@` only on its own file. |
| §2.3 deny-only | An allow naming a group the caller holds deny-only grants nothing; a deny naming it refuses. |
| §2.4 generic rights | Call `Authorize` with a mask holding a generic bit, on a file whose mode grants every right to the caller: assert refused. Set an entry whose mask holds a generic bit: assert `ErrInvalid`. The mapping itself is RFC 22's. |
| §4.1 what a mode bit grants | On a 0664 file owned by alice of group `staff`, as bob, a `staff` member, open asking the read and write sets with their minor rights: assert granted. Through that open set the times to the server's time and to a client time: assert both allowed and the client time stored as given; set a stored marker, the mode or the owner: assert `ErrPerm`. As bob on no open, set `Modify` to a client time: assert refused. As a user outside `staff`, the same open: assert refused. |
| §4.2 the two identities | **Unit**, on the functions: for every nine-bit `m` and both file types `d`, assert `ACLFromMode(m, d).Mode(o) == m`. For a generated corpus of ACLs — named users and groups, entries naming the owner, denies before and after allows, inherit-only and both-flag entries, owner rights, several allows for one special principal around an inherited deny, partial-write directory masks such as list and add a file only, and allows naming Authenticated Users and NETWORK — and each of two owners `o`, assert `a.WithMode(a.Mode(o), o, d)` grants every caller of a fixed population exactly what `a` grants. **Through both adapters**: on a 0750 directory alice owns with an allow for `builders`, run `chmod $(stat -c %a d) d` over NFS, then read the descriptor over SMB: assert the ACL unchanged and `builders` still granted. |
| §4.2, §4.3 the group bits are a mask | On a file owned by alice, group `staff`, set the ACL `allow bob READ_DATA`: assert `Mode` is 0040. `chmod` it to its own mode over NFS: assert a member of `staff` other than bob is still refused read, and the stored ACL unchanged. Set the same ACL on a file bob owns: assert `Mode` is 0400. |
| §4.3 the merge never raises | (a) Explicit `allow EVERYONE@ read`, inherited `deny bob write`, inherited `allow EVERYONE@ write`: `chmod` to the file's own mode over NFS; assert bob still refused write over both protocols and the ACL unchanged. Repeat with `GROUP@` and a member's deny. (b) A directory with `allow EVERYONE@` list and add a file: `chmod` to its own mode; assert the mask unchanged and a non-owner refused delete of another's file. (c) Inherited `deny bob read` and no `EVERYONE@` allow: `chmod o+r`; assert bob still refused, the new allow last. (d) An ACL holding a named-user allow granting write, an inherit-only entry, and an entry both effective and inheritable: `chmod 600`; assert the named user granted nothing, the inherit-only entry unchanged, the other split, and `Mode` the ACL's own. |
| §4.2 everyone-like principals | A file with `allow Authenticated Users R(rwx)` and `allow builders R(rwx)`: `chmod 750` over NFS. Assert an authenticated non-member is refused read over both protocols, `Mode` reads 0750, and `builders` keep `r-x`. Repeat with NETWORK and an asserted-identity caller. |
| §4.1 shown is evaluated | On a 0640 file with no stored ACL, query the descriptor over SMB and `acl` over NFSv4: assert both are `ACLFromMode(0640)`, and that each listed principal is granted exactly what its entry says. On 0604: assert a group deny is shown and a group member is refused read over both. |
| §2.8 no partial set | Set over SMB a DACL holding a callback deny for bob and an allow for everyone; assert the set is refused and the stored ACL unchanged, and bob's read still refused or granted as before. Repeat with an object allow, and with 129 entries. |
| §2.8 server-built lists | 100 effective and inheritable named entries, then `chmod`: refused `ErrInvalid`, ACL unchanged. 70 inheritable CREATOR OWNER entries with both flags: a `mkdir` below is refused `ErrNoSpace`. A `GETATTR` of `acl` with a small `maxresponsesize`: `NFS4ERR_REP_TOO_BIG`, never a shorter `acl`. |
| §5.2, Z8 round trip | Set from SMB an ACL naming a SID-only group with a deny, owner rights, CREATOR OWNER inherit-only, and a user with an NFS name. Read it over NFSv4 as an administrator, write back exactly what was read, read it over SMB: assert byte-equal descriptors. Then the reverse, starting from `nfs4_setfacl`. A translation that leaves out an entry it cannot name fails on the deny. |
| RFC 22 §11 fold | On a directory, set over NFSv4 `A:fd:OWNER@:rwx` and `A:fdg:GROUP@:rx`: read the descriptor over SMB and assert four entries, each owner or group SID followed by CREATOR OWNER or CREATOR GROUP inherit-only; write it back unchanged and read over NFSv4: assert the two original entries, byte-equal. Create a child: assert its entries are `OWNER@` and `GROUP@` either way. |
| §2.8 count | Set over NFSv4 64 effective, inheritable `OWNER@` entries with distinct masks: assert stored; read over SMB: assert 128 entries; write back: assert accepted and unchanged. Set 65: assert refused `ErrInvalid` over NFSv4 though it carries 65 entries. |
| §3.2 ceiling | bob's grant `read`; a file's ACL allows him full control: assert `Granted` over both protocols is the read row, a write open is refused, and a maximal open is granted read. As the file's owner on that share, set the ACL, `chmod`, `chgrp` to a group she is in, and `touch -d`: assert each refused over NFS; a DOS-attribute set and a DACL set over SMB: refused. As a privileged caller on a read-only admission: assert no write. |
| §3.1 read-only cause | On a share whose policy is read-only, a write by a caller whose grant is `read-write`: assert `ErrReadOnly` (a read-only filesystem on the wire). On a read-write share, a write by a caller whose grant is `read`: assert `ErrAccess`. |
| §3.1 through an open | Remove bob's grant, then write through his existing open: assert refused over both protocols. A close and an unlock through that open: assert they succeed. |
| §3.1 creating open | `open(O_CREAT\|O_WRONLY, 0444)` over NFS: assert the file created and the write through the open allowed. Over SMB, a `CREATE` with a `SecD` withholding write from the creator: assert the open granted its request. In a directory without add a file for the caller: assert the create refused. |
| §3.1 order | An immutable file in a directory granting bob delete a child: assert bob's delete refused. |
| §3.1 sticky | On a 1777 directory, store an ACL (written back unchanged over SMB, or one entry appended over NFSv4). As bob, remove alice's file in it over NFS and by delete on close over SMB: assert both refused, and alice's own removal succeeds. |
| §3.1 traverse | In a 0720 directory, a group member's `REMOVE` and `CREATE` over NFS and delete on close over SMB: refused. In 0730: allowed. |
| §3.1 `via` | `Granted` with a `via` that grants delete a child but holds no entry for the file: `DELETE` not granted. With a hard link in `via`: granted. |
| §3.4 maximal access | A maximal request alone where nothing is granted: refused. With write data named where only read is granted: refused. With nothing named and read granted: granted read. Run the same descriptors against a Windows server and assert the same answers. |
| §3.4 what an open holds | Open a file for read data only where the ACL grants full control: through that handle a write, a DACL set and a rename are refused. A backup-intent read-only open as an authenticated `admin`: a DACL set and an owner set are refused; opened asking write the ACL and change the owner with the intent: both succeed. |
| §3.4 privilege in access queries | On a mode-000 file owned by alice, on an export leaving root unsquashed: NFS `ACCESS` asking `READ` as uid 0 reports `READ`, and the read succeeds. Over SMB, a backup-intent open by an authenticated `admin` of a file whose DACL refuses it write: assert the open granted and `MaximalAccess` without write. |
| §3.6 audit selection | With the share's policy honouring file entries and an inheritable failed-write audit entry on the root: a refused write by bob over SMB and over NFS each emit one event; a granted write emits none; with the policy off, none. A privileged caller's refused execute emits one. With a successful-read entry: a maximal-only open granted read emits one. With a failed-write entry: a maximal-only open of a file granting bob nothing emits one. An `ACCESS` query emits none. |
| §3.5 system security | As a file's owner without `admin`: SMB query of the SACL is `STATUS_PRIVILEGE_NOT_HELD`, NFS `sacl` is refused. As an `admin` over `AUTH_SYS`, where `admin` is never set: refused. As an authenticated `admin`: both succeed, and an SMB open asking `ACCESS_SYSTEM_SECURITY` is granted with the right in its grant; a maximal request alone does not include it. Under an admission narrowed to read-only, as that `admin`: the open asking it is granted, a SACL query and a `sacl` `GETATTR` succeed, and a SACL set and a `sacl` `SETATTR` are refused. An open made before the narrowing cannot set the SACL after it. |
| §3.7 access-based enumeration | On a share asking it, a directory holding alice's folder and bob's, as bob over SMB: assert only bob's listed; over NFS: both listed. A privileged caller: both. |
| §3.8 locks outside an open | Over NFSv3's lock manager, as bob on alice's 0600 file: a read lock and a write lock refused. On a 0644 file: both allowed. |
| §4.4 inheritance | Each row of §4.4's table, over both protocols and against a Windows server. In alice's directory holding an inheritable "CREATOR OWNER: full" and "Finance: modify", bob creates a subdirectory and, in it, a file with mode 0777 and a umask of 022 carried apart from the mode: assert the subdirectory holds one effective and inheritable `OWNER@` full (spelled over SMB as bob's SID and CREATOR OWNER inherit-only), the file holds `OWNER@` full and nothing for alice, and Finance keeps modify on both. In a parent with no inheritable entry: the umask is applied. A parent without the auto-inherited flag: copies carry no inherited flag. A parent's protected flag is not passed down. |
| §4.4 create-time ACL | Put an inheritable failed-write audit entry on the root. As bob, `OPEN(CREATE)` with an `acl` attribute, and over SMB a `CREATE` with a `SecD` without a SACL: assert the inherited audit entry kept, and another caller's refused write emits one event. A `SecD` carrying a SACL from a non-admin: refused. |
| §2.3 kinds | A user entry for a principal whose UID equals a group's GID: assert a member of that group is not matched. |
| §2.7 stored order | Over SMB set `allow EVERYONE@ write data`, then `deny bob write data`: assert bob granted write over both protocols, and both reads return that order. A build that canonicalises fails. |
| §5.2 deleted principal | Delete a user named in a deny; read over NFS and SMB: assert the entry spelled with the null SID. Write back what was read: assert accepted, the entry gone, and `acl_set_normalised_total{kind="deleted_principal"}` up by one. |
| §2.8 lookup failure | Fail the principal lookup once, with an error other than `ErrUnknownPrincipal`, during an SMB set naming a SID: assert the set refused with that error, not `ErrInvalid`, and the stored ACL unchanged. |
| Z1, §2.4 masks | Set over NFSv4 an allow whose mask holds `0x200`, `0x01000000` and a reserved bit: assert the stored mask holds none of them, and the clearing counted. |
| Z9 | For every principal kind an entry can name (§5.2), and every well-known row, read a one-entry ACL over both protocols: assert no error and no entry missing. |
| Z11 | Set a directory's DACL over SMB with the auto-inherit request, and its `acl` over NFSv4: assert every child's `Version` unchanged. |

**What must not stand in.** A round trip through the translation alone, without
the store between the write and the read, is not evidence for Z8: it proves only
that the two halves agree with each other. A check against the mode-only path is
not evidence for the ACL path, and the reverse.

## 10. Open questions

1. **Maximal requests with generic bits.** Samba grants an SMB `MAXIMUM_ALLOWED`
   open naming `GENERIC_READ` or `GENERIC_EXECUTE` even when not every right
   they map to is granted. Whether Windows does, which §3.4's rule decides
   against, is to be settled by the §9 run against a Windows server.
2. **A `who` that is a SID.** §5.2's spelling is untested against the NFSv4
   ACL tools of every client the reference profile lists; the decision note
   names the fallback.
3. **Audit list order and flags in `acl`.** NFSv4.0 carries both lists in one
   attribute with no flags; whether a client that interleaves audit and access
   entries expects their relative order back is unmeasured. The model drops it.
4. **Mandatory labels.** Whether any Windows workload on the reference profile
   sets a mandatory-label entry on files, which RFC 22 §11 refuses, is unmeasured.
5. **Allows holding part of what the write bit adds.** `Mode` reads the write
   bit from write data alone, and a merge limiting to bits without write removes
   append, write attributes and write extended attributes too. So an allow
   holding append without write data — `allow bob APPEND_DATA` — implies no
   write bit, and `chmod` to the file's own mode strips the append: §4.2's second
   identity does not hold for it, and is stated without it.
6. **`dacl` and `sacl`.** Whether any client of the reference profile reads or
   sets them is unmeasured. If none does, serving `acl` alone loses no round
   trip, because `acl` leaves the stored flags as they were (RFC 21 §11.3). What it does
   lose is setting the protected flag over NFS.
7. **Re-deciding a durable or persistent reconnect.** RFC 14's grace reclaim is
   authorised like a new open against the file as it is now; nothing says what
   an SMB durable or persistent reconnect does. Re-running §3.1 with the open's
   original request would refuse a reconnect whose ACL has since narrowed;
   keeping the stored grant, as Windows does, holds the old rights until the
   durable timeout (the per-call ceiling still applies, §3.1). To be settled by a
   test against a Windows server.
8. **Audit entries and deny-only principals.** Whether an audit entry naming a
   group selects a decision for a caller holding that group deny-only, as
   Windows is believed to do for a filtered administrator token, is unverified;
   §3.6 states no rule. To be settled by a test against a Windows server.
9. **A non-owner clearing setuid and setgid by `SETATTR`.** RFC 7 refuses a mode
   set by a non-owner with `ErrPerm`. If a current client sends `mode = current
   &^ 06000` before a write, as some clients are said to, a group writer of a
   setgid file would be refused every such write. Whether any client does needs a
   client test; if one does, a set whose only effect is clearing those bits is
   allowed to any caller holding write data.

## Appendix A — prior art

*Non-normative.*

- **Windows' access check** (MS-DTYP 2.5.3.2) — the owner's implicit rights
  before the DACL, owner rights, the ordered allow and deny walk, `MAXIMUM_ALLOWED`;
  the generic mapping for files (MS-FSA 2.1.5.1.2); a create's own open granted
  its request (MS-FSA 2.1.5.1.1); inheritance (MS-DTYP 2.5.3.4) and propagation
  done by the caller walking the tree.
- **NFSv4 ACLs** ([RFC 8881 §6](https://www.rfc-editor.org/rfc/rfc8881.html#section-6))
  — the shared mask bits, the `acl`, `dacl` and `sacl` attributes, the mode and
  ACL interaction of §6.4, inheritance of §6.4.3, the sticky bit of §6.2.1.3.2.
- **POSIX ACLs** — the mask entry shown in a file's group bits, which §4.2's
  group class follows, and the rule that `chmod` of the group bits sets the mask.
- **Samba's NFSv4 ACL mapping** — maps an entry for the owner's SID to the NFSv4
  owner special principal under some of its settings, the model for RFC 22 §11's owner fold; its
  share ACLs are a ceiling intersected with the file ACL, the model for §3.2.
- **The Linux NFS server** — translates NFSv4 ACLs to POSIX ACLs and refuses
  what POSIX cannot hold, rather than storing less: the model for §2.8; lets its
  lock manager through for read access or ownership, the model for §3.8.
- **ZFS and its NFSv4 ACLs** — one ACL model served to both NFS and SMB, with
  a property choosing how `chmod` treats an ACL, and the sticky bit enforced
  alongside an ACL; the reason §4 fixes one rule rather than a setting.

## Appendix B — where the current code differs

*Non-normative.* The code on the branch this RFC was written against, by section.

| § | This RFC | The code today |
| --- | --- | --- |
| §2.3 | principals by ID, kind part of the entry | an entry's principal is a string: `OWNER@`-style specials, `sid:<SID>`, `name@domain`, or `<number>@localdomain`; the last matches the caller's UID **or** any of its GIDs, so a user entry for uid 1000 matches every member of gid 1000 |
| §2.3, §3.3 | owner rights leaves `OWNER@` matching | an effective owner-rights entry stops `OWNER@` entries matching the owner, as Samba does |
| §2.6 | null DACL stored as one allow-everyone entry | stored as a flag the evaluator checks first |
| §2.6 | the defaulted flag records where a list came from | an `ACL.Source` field records it |
| §2.8 | a SID no principal holds refuses the set | a foreign-domain SID in a DACL entry is stored as `sid:<SID>` and round-trips |
| §2.8 | an unsupported entry refuses the set | the descriptor parser skips entry types it cannot map, object and callback denies included, and stores the rest |
| §3.1 step 5 | the owner's implicit rights are not removable by a deny | an explicit deny removes them per bit; an administrator SID owner also receives change-the-owner implicitly, as a take-ownership privilege (`HasTakeOwnershipPrivilege`, `IsAdminSID`) |
| §3.1, §3.5 | privilege is a flag of the caller or of an open | privilege is derived from `UID == 0` throughout the permission checks, the access checker, the setattr and create paths and the SMB enumeration fast path |
| §3.1, RFC 7 §7.1 | the share ceiling is applied at the chokepoint | the SMB write and create handlers test the session's share permission themselves; the metadata layer separately strips write and delete for a read-only grant, and a handle-based write bypass skips the file check unless the ACL holds a deny |
| §3.2 | the ceiling is never written into an ACL | share grants are synthesised into the share root's ACL, and direct SID grants are added to the descriptor shown for files with no stored ACL, display only (`BuildShareRootACL`, `LocalDomainPrincipal`) |
| §3.4 | maximal requests as Windows' access check | `GENERIC_READ` and `GENERIC_EXECUTE` beside `MAXIMUM_ALLOWED` are granted best-effort, as Samba does |
| §3.6 | audit entries select events | audit entries are stored and shown and select nothing |
| §3.7 | access-based enumeration decided at the chokepoint | the SMB handler filters listings itself, with its own `UID == 0` bypass |
| §4.1 | the mode-implied ACL is shown and evaluated | a file with no stored ACL is shown owner and SYSTEM full control over SMB and an empty `acl` over NFSv4, and evaluated by a separate mode check in which the write bit grants delete on the file and ownership grants change-the-owner (`SynthesizeWindowsDefault`) |
| §4.2 | setting an ACL stores the mode it implies | setting an ACL leaves `Mode` unchanged; the mode-from-ACL function has no production caller |
| §4.3 | `chmod` limits named allows and never raises one | `chmod` rewrites only the owner, group and everyone entries and leaves named entries unchanged |
| §4.4 | the inherited flag follows the parent's auto-inherited flag | the auto-inherited flag is kept only when a set carries both it and the auto-inherit request, unless a per-share setting (`acl flag inherited canonicalization = no`) keeps it as sent; inheritance past the size limit is truncated, oldest first |
| §4.4 | no server-side propagation | a propagation function exists with no production caller, and the SMB fidelity guide lists recursive propagation as working (`PropagateACL`) |
| §5.1, RFC 20 §2.3 | one translation both adapters share | NFS writes stored who strings verbatim; SMB has its own principal-to-SID mapping inside its handler package |
| §5.1, Z6, Z8 | every enforced entry is shown by both protocols | an entry may carry an `NFSNumericTwin` marker: a hidden enforced entry one protocol shows and the other does not |
| RFC 22 §11 | an unresolvable owner SID refuses the set | it is ignored and the rest of the set applied |
| RFC 21 §11.3 | `acl`, `dacl`, `sacl` | only `acl` is served; it carries no audit entries on read, and audit entries sent in it are stored in the access list; `aclsupport` reports all four kinds |
| RFC 21 §11.3 | the group flag from the principal's kind | the group flag is neither set on read nor read on write |
| RFC 21 §8.1 | `ACCESS` bits by right | a directory's `MODIFY` and `EXTEND` both map to one write permission needing write data and append together |
| docs | — | `docs/internals/acl-design.md` still states that ACLs are validated for canonical order, which the code no longer does |
