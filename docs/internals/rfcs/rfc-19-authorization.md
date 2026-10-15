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
aliases:
  - RFC 19
  - authorization
tags:
  - rfc
---
# RFC 19 — authorization

**Status:** draft. [§12](#12.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone changing how a permission is decided, how an ACL is
spelled on the wire, or how `chmod` and an ACL agree — and anyone writing the
translation between a security descriptor or an NFSv4 ACL and the stored ACL.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix B](#Appendix%20B%20%E2%80%94%20where%20the%20current%20code%20differs) says where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

[RFC 18](rfc-18-identity.md) answers **who** the caller is: a resolved
principal, with its UID, its groups, its SIDs, and two flags saying what its
flavour proved. This RFC answers **what that principal may do to a file**.

Every file has one access-control list (ACL): an ordered list of entries, each
saying "allow" or "deny" some rights to some principal. A file without a stored
ACL still has one: the one its POSIX mode implies. Windows clients read and
write that list as a *security descriptor*; NFSv4 clients read and write it as
the `acl` attribute. Both are spellings of the same stored list. The rights
themselves — read data, write data, delete, change the ACL — have the same bit
values in both protocols, by design of NFSv4; what differs is how a principal is
named and where three flag bits sit.

Where the list is stored, and the one function that consults it, belong to
[RFC 7](rfc-7-namespace-metadata.md). This RFC owns what the list means: the
model, the order a decision is made in, how the mode and the list agree, and
the two spellings.

**The problem, with one example.** alice opens the Security tab on
`\\dittofs\builds\release` from alice-pc and adds three entries: deny bob
write, allow herself full control, allow the `builders` group modify. build01,
a member of `builders`, reaches the same directory over NFSv4.

Suppose each adapter translated on its own. The NFS adapter meets an entry for
a principal it has no `user@domain` name for and, finding nothing to print,
leaves it out of the `acl` it returns. An operator on build01 runs
`nfs4_setfacl -a` to add one more entry; the tool reads the ACL, appends, and
writes the whole list back. bob's deny is gone. Nothing failed: the NFS reply
was well formed, the Windows view still showed the deny until the write, and
bob can now overwrite the release. The defect sat in the translation nobody
compared with the other.

With this RFC:

1. Both adapters call one translation (§5). Every principal has a spelling in
   both protocols: a principal NFS has no name for is spelled by its SID
   (§5.2), so the deny reaches build01 and goes back intact.
2. The entries are stored by principal ID, in the order alice's client sent
   them (§2.7). Neither adapter reorders, drops or rewrites them.
3. bob's write over SMB and over NFS runs the same walk (§3.1): the deny comes
   first, his `builders` membership grants nothing it covered, and both are
   refused.

![One stored ACL in the middle, spelled two ways: on the left the SMB security descriptor alice-pc reads, with SIDs and Windows flag bits; on the right the NFSv4 acl build01 reads, with who strings, letters and the group flag; arrows through one shared translation; bob's write refused over both](img/rfc19-two-views.svg)

**The words you need.**

- **ACL** — a file's one ordered list of entries, plus a second list of audit
  entries and a few flags ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)).
- **entry** — one allow, deny, audit or alarm statement: a kind, a principal, a
  set of rights and some flags (§2.2). Windows calls it an ACE.
- **right** — one thing a caller may be allowed to do, such as read data or
  delete (§2.4). A request asks for a set of rights.
- **the walk** — the evaluation of the entries in stored order for one caller
  ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode), §3.3).
- **share ceiling** — the most any caller may be granted on a share, set by its
  share grant and narrowed by admission (§3.2).
- **security descriptor** — the Windows structure holding owner, group, DACL
  (the allow and deny entries) and SACL (the audit entries) (§6).
- **the translation** — the one pure function pair, shared by both adapters,
  between a wire spelling and the stored ACL (§5).

**What this RFC promises.**

- One model, defined once: every right, principal and flag either protocol can
  express is either in it or refused with a stated reason. Nothing is dropped
  silently.
- One decision for one request, whatever protocol it came in on: the same
  rights asked of the same ACL by the same principal get the same answer.
- What a client is shown is what is enforced: the descriptor or `acl` returned
  is the ACL the walk reads, never a decoration.
- A read-modify-write through either protocol loses no entry, no deny and no
  inheritance flag.
- The mode and the ACL agree after every change, by two functions this RFC
  defines (§4).

**How the rest is organised.** §1 gives the purpose and the split with RFC 7.
§2 is the model, §3 evaluation, §4 the mode and inheritance. §5 is the shared
translation, §6 and §7 its two spellings. §8 is the worked example end to end.
§9–§11 are invariants, metrics and conformance; §12 lists open questions.
Appendix A is prior art, Appendix B where the current code differs.

### Scenarios

**build01 runs `chmod 750` on a directory alice permissioned from Windows.**

1. `SETATTR` of `mode` reaches the chokepoint as a `chmod` on a file with a
   stored ACL.
2. RFC 7's merge rewrites the owner, group and everyone entries and limits
   every named allow entry to the new group bits
   ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)); bob's
   deny is untouched.
3. The stored `Mode` is the mode the merged ACL implies (§4.2), written in the
   same transaction.

Outcome: `ls -l` and the Security tab show the same permission, and the deny
survives a `chmod`.

**An auditor wants every failed write to `profiles` recorded.**

1. An administrator of the share adds an audit entry — everyone, write data,
   failed access — to the share's root, inheritable (§3.7).
2. The share's audit policy is set to honour file audit entries (§3.7).
3. bob's refused write to a profile container matches the entry; the chokepoint
   emits an access event, which [RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)
   carries on.

Outcome: auditing is the same over SMB and NFS, because it is selected where the
decision is made, not in an adapter.

**bob holds read on the share, and a file's ACL grants him full control.**

1. bob's share grant on `builds` is `read`; the file's ACL allows him full
   control.
2. The walk grants full control; the share ceiling then narrows it to the read
   rights (§3.2).
3. His open asking for write is refused; one asking `MAXIMUM_ALLOWED` is granted
   read.

Outcome: a file can narrow a share grant and never widen it.

**A tool sets a descriptor with a conditional deny entry.**

1. The descriptor carries a callback deny entry, a kind the model does not hold
   (§6.4).
2. Dropping it would store the rest and grant what the deny forbade, so the set
   is refused whole with `ErrInvalid`; nothing is stored.

Outcome: an unsupported entry costs the client an error, never an open door.

## In short

- One ACL per file, stored by [RFC 7](rfc-7-namespace-metadata.md); this RFC
  owns what it means. Entries name principals by ID, never by a protocol
  spelling.
- Rights are the union both protocols need, at the bit values both protocols
  already share. Generic rights are mapped before anything is stored or asked.
- A decision runs one sequence: map the request, take the share ceiling, apply
  privilege or the owner's implicit rights and the walk, add what the parent
  grants for delete, remove what the flags refuse, intersect with the ceiling,
  decide, and select audit events.
- The walk is RFC 7's ordered allow and deny walk, in stored order, adopted as
  the evaluation; the server never reorders what a client set.
- A file with no stored ACL is evaluated against the ACL its mode implies, and
  that same ACL is what both protocols are shown.
- The mode an ACL implies is computed by one function; a `chmod` merges by
  RFC 7's rule; the two agree after every transaction.
- One translation, shared by both adapters, spells the ACL as a security
  descriptor and as an NFSv4 `acl`, `dacl` or `sacl`. Every principal has a
  spelling in both; what cannot be held is refused, never dropped.
- Audit entries never grant or deny. They select access events, when the
  share's audit policy says to honour them.

---

## 1. Purpose

Two protocols that describe permissions differently, served from one store,
need one meaning for a stored ACL, or each adapter grows its own. A second
meaning is where a hole hides: an entry one adapter cannot show is an entry the
other side's clients delete without knowing it existed; a right one protocol
reports and the other enforces differently is a right a client was told it has
and does not.

This RFC fixes that meaning once, and the two spellings of it.

### 1.1 Non-goals

This RFC **MUST NOT**:

- decide who the caller is: authentication, flavours, the mapping between UIDs,
  SIDs and names, squashing, and who is an administrator are
  [RFC 18](rfc-18-identity.md)'s. The identity arrives resolved
  ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved));
- decide where a permission is checked or how often: the one chokepoint, the
  open-time grant and the per-call share grant are RFC 7's
  ([§7.1](rfc-7-namespace-metadata.md#7.1%20One%20chokepoint),
  [§7.2](rfc-7-namespace-metadata.md#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always))
  and [RFC 17 §4.6](rfc-17-vfs.md#4.6%20One%20chokepoint)'s;
- decide admission — a share's state, flavour rule and client rules —
  which is [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s;
- define how access events are delivered, kept or exported, which is the
  planned RFC 25's;
- define the wire encoding of either protocol beyond the fields that carry an
  ACL, or the status code of each refusal, which are RFC 21's and RFC 22's.

### 1.2 What RFC 7 holds and what this RFC holds

RFC 7 was written first and fixed an interim evaluation so the chokepoint could
be built. This RFC adopts it rather than replacing it, and owns the rest. Where
RFC 7 states a rule in full, this RFC cites it and does not restate it.

| Topic | Owner | Where |
| --- | --- | --- |
| the ACL entity, its storage, and that `Mode` and ACL change in one transaction | RFC 7 | [§2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode) |
| the chokepoint, open-time grants, the decision against the file, the resolved identity, the privileged flag and who may set owner and group | RFC 7 | [§7.1–§7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved) |
| cache keys of decisions | RFC 7 | [§7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read) |
| emitting access events, and the immutable and append-only flags | RFC 7 | [§7.6](rfc-7-namespace-metadata.md#7.6%20Decisions%20are%20observable), [§7.8](rfc-7-namespace-metadata.md#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint) |
| what removing a name needs, and delete decided with the parent | RFC 7 | [§7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs) |
| the ordered walk, the owner's implicit rights, generic rights, what a mode bit grants, the well-known principals, inheritance at create, maximal access, the `chmod` merge | RFC 7, adopted here | [§2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode) |
| the model: entry kinds, principals, rights and their bits, flags, order, what a stored ACL may hold | this RFC | §2 |
| the sequence one decision runs, the share ceiling, `MAXIMUM_ALLOWED`, the system-security right, which decisions an audit entry selects | this RFC | §3 |
| the ACL a mode implies, the mode an ACL implies, how inheritance is computed and set | this RFC | §4 |
| the shared translation and both spellings | this RFC | §5–§7 |

> decision: RFC 7's walk is adopted unchanged. It is what NFSv4 clients assume
> and what Windows' access check does for allow and deny entries, and RFC 7's
> table of cases is already run against a Windows server
> ([RFC 7 §12.1](rfc-7-namespace-metadata.md#12.1%20Group%20A%20%E2%80%94%20wrong%20file%2C%20lost%20file%2C%20wrong%20caller)).
> Replacing it would buy nothing a client can observe. Overturned by a case of
> that table, or of §11 here, that a Windows server answers differently.

## 2. The model

### 2.1 An ACL

An ACL is two ordered lists and their flags
([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)'s
`ACL`):

- the **access list** — allow and deny entries, which the walk reads;
- the **audit list** — audit and alarm entries, which never grant or deny and
  are read only to select access events (§3.7);
- each list's **flags** (§2.6).

Owner and group are not part of the ACL. They are fields of the File, which
the ACL refers to through the special principals `OWNER@` and `GROUP@` (§2.3).

### 2.2 Entry kinds

| Kind | List | Effect |
| --- | --- | --- |
| allow | access | grants the rights it covers that no earlier entry decided |
| deny | access | refuses the rights it covers that no earlier entry decided |
| audit | audit | selects an access event for a decision it matches (§3.7) |
| alarm | audit | as audit; there is no separate alarm channel |

No other kind is held. Object, callback (conditional), mandatory-label,
resource-attribute and scoped-policy entries, which security descriptors can
carry, are refused at the translation (§6.4).

> ponytail: an alarm entry is an audit entry. Neither protocol defines what an
> alarm does beyond "raise an alarm", and the access-event stream is the one
> channel the service has. Give alarms their own event type when the
> observability RFC defines a second consumer that must tell them apart.

### 2.3 Principals

An entry names its principal by ID ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)),
of one of three kinds:

- **a user or a group** — a minted principal, matched when it is the caller or
  one of the caller's groups. Its kind is part of the entry: a user entry never
  matches a caller through a group of the same number, and a group entry never
  matches a user.
- **a special principal** — `OWNER@`, `GROUP@`, `EVERYONE@`, which match the
  file's current owner, members of the file's current group, and every caller.
- **a well-known principal** — owner rights, Authenticated Users, BUILTIN\Users,
  Administrators, NETWORK, SYSTEM, CREATOR OWNER and CREATOR GROUP, each with
  the fixed ID of [RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)
  and the matching rule of
  [RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)'s
  table, which this RFC adopts. A well-known principal with no row there matches
  no caller; a deny naming one is refused when stored, and an allow naming one
  is stored and grants nothing (RFC 7).

Matching reads the identity [RFC 18](rfc-18-identity.md) resolved — the
principal, its groups, the `authenticated` and `admin` flags — and nothing
else. It never reads a UID, GID, SID or name: those are spellings, held on the
User and Group records and in the protocol-ID index, and resolved to principals
by RFC 18 before an entry is stored or a caller arrives. A caller resolved as
the share's anonymous principal is that principal: it matches `EVERYONE@`,
entries that name it, and `OWNER@` only on a file it owns.

`OWNER@` and `GROUP@` follow the file: after a `chown` they match the new owner.
An entry naming the owner's own principal does not; it keeps matching that
principal. The two are different entries, and §6.3 keeps them apart on the
Windows side, which has no `OWNER@`.

### 2.4 Rights

A right is one bit of an entry's mask and of a request. The bit values are the
ones NFSv4 and Windows already share, so a mask crosses the translation
unchanged. On a directory three bits carry a second name.

| Right | On a directory | Bit | NFSv4 · SMB |
| --- | --- | --- | --- |
| read data | list | `0x00000001` | `READ_DATA` / `LIST_DIRECTORY` · `FILE_READ_DATA` / `FILE_LIST_DIRECTORY` |
| write data | add a file | `0x00000002` | `WRITE_DATA` / `ADD_FILE` · `FILE_WRITE_DATA` / `FILE_ADD_FILE` |
| append | add a subdirectory | `0x00000004` | `APPEND_DATA` / `ADD_SUBDIRECTORY` · `FILE_APPEND_DATA` / `FILE_ADD_SUBDIRECTORY` |
| read extended attributes | | `0x00000008` | `READ_NAMED_ATTRS` · `FILE_READ_EA` |
| write extended attributes | | `0x00000010` | `WRITE_NAMED_ATTRS` · `FILE_WRITE_EA` |
| execute | traverse | `0x00000020` | `EXECUTE` · `FILE_EXECUTE` / `FILE_TRAVERSE` |
| delete a child | | `0x00000040` | `DELETE_CHILD` · `FILE_DELETE_CHILD` |
| read attributes | | `0x00000080` | `READ_ATTRIBUTES` · `FILE_READ_ATTRIBUTES` |
| write attributes | | `0x00000100` | `WRITE_ATTRIBUTES` · `FILE_WRITE_ATTRIBUTES` |
| delete | | `0x00010000` | `DELETE` · `DELETE` |
| read the ACL | | `0x00020000` | `READ_ACL` · `READ_CONTROL` |
| write the ACL | | `0x00040000` | `WRITE_ACL` · `WRITE_DAC` |
| change the owner | | `0x00080000` | `WRITE_OWNER` · `WRITE_OWNER` |
| synchronize | | `0x00100000` | `SYNCHRONIZE` · `SYNCHRONIZE` |

These fourteen are the **entry rights**: the only bits a stored entry's mask
holds. Three more bits may appear in a request and never in a stored mask:

- **system security** (`ACCESS_SYSTEM_SECURITY`, `0x01000000`) — read or write
  the audit list. No entry grants it (§3.5).
- **maximum allowed** (`MAXIMUM_ALLOWED`, `0x02000000`) — a request flag, not a
  right: "grant me whatever I may have" (§3.4).
- **generic rights** (`0xF0000000`) — mapped to entry rights by the file
  generic mapping
  ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode))
  before a request is evaluated and before an entry is stored. This RFC owns
  that mapping; RFC 22 §6.1's interim rule is this one.

A mask a client sets is reduced to entry rights before it is stored: generic
bits are mapped, and every other bit outside the fourteen — system security,
maximum allowed, NFSv4's retention bits (`0x00000200`, `0x00000400`), reserved
bits — is cleared and counted (§10). Clearing a bit no request can be granted
by narrows neither an allow nor a deny in any way a request can observe.

What each operation asks for is [RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)'s
and [RFC 17](rfc-17-vfs.md)'s.

### 2.5 Entry flags

| Flag | Meaning | NFSv4 bit | SMB bit |
| --- | --- | --- | --- |
| file inherit | copied to new files below a directory | `0x01` | `0x01` (`OI`) |
| directory inherit | copied to new subdirectories | `0x02` | `0x02` (`CI`) |
| no propagate | copied one level only | `0x04` | `0x04` (`NP`) |
| inherit only | governs children, not this file | `0x08` | `0x08` (`IO`) |
| inherited | this entry came from a parent | `0x80` | `0x10` |
| successful access | an audit entry selects allowed decisions | `0x10` | `0x40` |
| failed access | an audit entry selects refused decisions | `0x20` | `0x80` |

The bits differ for the last three, and the translation **MUST** map each by
name; a mask copied byte for byte turns NFSv4's inherited flag into Windows'
failed-access flag. NFSv4's group flag (`ACE4_IDENTIFIER_GROUP`, `0x40`) is not a
flag of the model: it is how NFSv4 spells the principal's kind (§7.2). An entry
flag outside the table is cleared and counted.

An entry is **effective** on a file when it is not inherit-only. Only effective
entries are walked, and only effective audit entries select events.

### 2.6 ACL flags, null and empty

Each list carries three flags, the same in both protocols:

| Flag | Meaning | SMB control bit (DACL · SACL) | NFSv4.1 `aclflag4` |
| --- | --- | --- | --- |
| protected | entries are not inherited into this list from a parent (§4.4) | `SE_DACL_PROTECTED` · `SE_SACL_PROTECTED` | `ACL4_PROTECTED` |
| auto-inherited | the inherited entries were computed by the inheritance rules | `SE_DACL_AUTO_INHERITED` · `SE_SACL_AUTO_INHERITED` | `ACL4_AUTO_INHERIT` |
| defaulted | the list came from a default, not from a client | `SE_DACL_DEFAULTED` · `SE_SACL_DEFAULTED` | `ACL4_DEFAULTED` |

An **empty** access list grants nothing but the owner's implicit rights
([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)),
as both protocols specify. A Windows **null DACL** — a descriptor saying it
carries a DACL and carrying none — grants everyone everything; it is stored as
one entry, allow `EVERYONE@` every entry right, and read back as that entry, not
as a null DACL.

> ponytail: a null DACL is not a stored state. One allow-everyone entry decides
> every request exactly as a null DACL does, and keeps the walk free of a flag
> to check first. The cost is that a client reads back a one-entry DACL where it
> wrote a null one. Add the state when a client is shown to depend on reading a
> null DACL back.

### 2.7 Order

The access list is walked in **stored order**, and the stored order is the order
the client sent. The server **MUST NOT** reorder, merge or deduplicate entries
a client set: a later allow after a deny means something different from the
same allow before it, and the client chose.

*Canonical order* — explicit deny, explicit allow, inherited deny, inherited
allow, each inherited group in its order of inheritance — is what Windows tools
write and what the server writes whenever it builds a list itself: the ACL a
mode implies (§4.1), an inherited ACL (§4.4), and RFC 7's `chmod` merge, which
rewrites entries in place. A list need not be canonical to be stored or walked.

The audit list has no order that matters: an audit entry never decides.

### 2.8 What a stored ACL may hold

A set **MUST** be refused whole, with `ErrInvalid`, and nothing stored, when:

- either list holds more than 128 entries, or its encoding is larger than
  64 KiB (both fixed values);
- an entry is of a kind §2.2 does not hold;
- an entry's principal cannot be resolved to a principal ([RFC 18](rfc-18-identity.md));
- a deny names a well-known principal with no row in RFC 7's table.

A set **MUST NOT** be accepted with one entry left out, but for an entry naming
a deleted principal (§5.2). Leaving out an allow
loses a grant the client meant; leaving out a deny grants what the client
forbade — the failure is open, and the client is told it succeeded.

> decision: a SID no principal holds — a user of a domain the installation
> does not know — refuses the set, rather than being stored as a bare SID the
> walk would compare. An entry stores a principal ID and nothing else
> ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)); a bare SID
> would be a second, unmanaged identity space. The cost: copying a descriptor
> from another domain's server, as `robocopy /SEC` does, fails on its first
> foreign entry. Overturned by RFC 18 minting a principal for a well-formed
> foreign SID, after which such an entry stores like any other.

## 3. Evaluation

### 3.1 One request, in order

`Authorize(want)` and `Granted` ([RFC 7 §11.1](rfc-7-namespace-metadata.md#11.1%20Interface))
run this sequence, on the file the operation will act on
([RFC 7 §7.3](rfc-7-namespace-metadata.md#7.3%20The%20decision%20is%20against%20the%20file%2C%20not%20the%20name)),
after the service has admitted the call and the identity is resolved:

1. **Map the request.** Generic bits become entry rights (§2.4). Maximum
   allowed and system security are set aside for steps 9 and 4.
2. **Take the share ceiling** `C` (§3.2). No grant admits the caller: refused,
   `ErrAccess`.
3. **Privilege.** A privileged caller
   ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved))
   is granted every entry right — except execute on a file that no execute bit
   and no entry grants to anyone, as RFC 7 states — and skips steps 5 and 6.
4. **System security**, if asked: granted only by §3.5; otherwise the request
   is refused whatever else it asked.
5. **The owner's implicit rights**, before any entry is read
   ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)):
   read and write the ACL, unless an effective owner-rights entry exists.
6. **The walk** over the stored access list, or over the ACL the mode implies
   when none is stored (§4.1) (§3.3).
7. **The parent.** On a call that names the directory it resolved through,
   delete is granted when that directory grants delete a child, and read
   attributes when it grants list, the sticky rule included
   ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)).
8. **The flags** of [RFC 7 §7.8](rfc-7-namespace-metadata.md#7.8%20Immutable%20and%20append-only%20flags%20are%20enforced%20at%20the%20chokepoint)
   remove what they refuse.
9. **Intersect with the ceiling**: the granted set `G` is what steps 3–8
   granted, within `C`. `Granted` returns `G`. `Authorize` succeeds exactly
   when every right in `want` is in `G`, and is refused `ErrAccess` otherwise;
   with maximum allowed, as §3.4 says.
10. **Select audit events** for the outcome (§3.7).

![The ten steps one request runs, shown for bob's SMB open asking GENERIC_WRITE on release/notes.txt: mapped to FILE_GENERIC_WRITE, ceiling read-write, not privileged, not the owner, the walk denies write data at bob's deny entry, the parent and flags change nothing, the request is refused, and an audit entry selects the refusal](img/rfc19-evaluation.svg)

The order is part of the rule. The ceiling applies after privilege, so a
privileged caller on a read share is still read-only. The parent's delete is
added before the flags, so an immutable file refuses delete whatever its parent
grants. The owner's implicit rights are added before the walk and are not
removable by a deny, and the ceiling still narrows them, so the owner of a file
on a read share cannot rewrite its ACL.

### 3.2 The share ceiling

A share grant names one principal's access to one share
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
A caller's **grant level** is read from the share's grants by
[RFC 18](rfc-18-identity.md)'s order — its own grant first, then the widest of
its groups', then authenticated users', then everyone's — and arrives with the
identity. The **share ceiling** is the rights that level allows, narrowed by
admission:

| Level | Ceiling |
| --- | --- |
| none (no grant) | nothing: the call is refused at step 2 |
| read | read data and list, execute and traverse, read attributes, read extended attributes, read the ACL, synchronize |
| read-write | every entry right |
| admin | every entry right; on an authenticated flavour the identity's `admin` flag is set ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)), which Administrators entries match and the system-security right needs (§3.5) |

Admission may narrow a grant to read-only — a client rule, or a share marked
read-only ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)).
The chokepoint does not evaluate client rules
([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)),
so the narrowing **MUST** arrive as a field of the identity the service hands
it, and the ceiling is then the read row's whatever the grant. Because the field
is part of the identity, every cache key over the identity includes it
([RFC 7 §7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read)).

A file's ACL can narrow what the ceiling allows and never widen it: the
ceiling is an intersection, not an entry. The ceiling is never written into a
file's ACL, stored or shown, and a share's root carries its own ACL like any
directory.

> decision: `read-write` is every entry right, the ACL's included, unlike
> Windows' "Change" share permission, which withholds writing the ACL and the
> owner. NFS has no share-level split: `chmod`, `chgrp` and `cp -p` on one's own
> files write the ACL, and a ceiling without it would break every NFS client of
> a read-write share. Overturned when an operator needs a share whose users
> write data and may not re-permission it; that is a fifth level, not a change
> to this one.

> decision: the levels are a ceiling only. A file ACL cannot see the share
> grant, and the grant never sees the file: the two are intersected at step 9
> and nowhere else. Overturned if an operator needs a share grant that also
> grants, which would make the ceiling an entry and break Z5.

### 3.3 The walk

The walk is [RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)'s,
adopted unchanged: effective entries that match the caller (§2.3), in stored
order; an allow grants the requested rights it covers that no earlier entry
decided, a deny refuses them; a right no entry granted is refused. `Granted`
runs the same walk over every entry right without stopping early.

Three cases the walk meets, stated here because RFC 7 does not:

- **An entry naming a principal since deleted** matches no caller: no identity
  resolves to a deleted principal. A deny naming one therefore refuses no one.
  The ID is never reissued ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)),
  so no later account inherits the entry, allow or deny.
- **An owner-rights entry and `OWNER@`.** An effective owner-rights entry
  removes the owner's implicit rights; it does not stop `OWNER@` entries
  matching the owner. Both are walked, in order.
- **The ACL a mode implies** is walked like any stored one. A file with no
  stored ACL has no second check path.

### 3.4 Maximum allowed

An SMB open whose `DesiredAccess` holds `MAXIMUM_ALLOWED` is granted `G` (§3.1
step 9), and **MUST** be refused `ErrAccess` when `G` is empty or when any right
it asked for explicitly, after generic mapping, is outside `G` — as Windows'
access check decides. The open's grant, stored with the open
([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), is `G`.

NFS `ACCESS`, SMB's `MaximalAccess` and the `MxAc` create context answer `G` too
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), computed without the privileged
flag ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)).

### 3.5 Privilege and the system-security right

Privilege is RFC 7 §7.4's one flag, set by the service on the callers RFC 7
names; this RFC only places it in the sequence (§3.1 step 3). Take-ownership is
not a privilege: it is the change-the-owner right, which an entry grants.

The **system-security right** — reading or writing the audit list — is granted
only to a caller whose identity has `admin` set, and never by an entry, by
ownership or by privilege alone. Over SMB a request for it without `admin` is
`STATUS_PRIVILEGE_NOT_HELD`
([RFC 22 §11](rfc-22-smb.md#11.%20Security%20descriptors)); over NFS, `sacl`
and the audit entries of `acl` are gated the same way
([RFC 21 §8.1](rfc-21-nfs.md#8.1%20Supported)). Under an admission narrowed to
read-only, an administrator may read the audit list and a set of it is refused.

### 3.6 Deleting and renaming

Removing a name is decided by RFC 7 §7.7 inside the operation's transaction:
delete on the file, or delete a child on its directory. This RFC adds nothing to
that rule but the rights' names (§2.4) and the sequence (§3.1), in which the
parent's grant is step 7.

### 3.7 Audit entries select events

The chokepoint emits an access event ([RFC 7 §7.6](rfc-7-namespace-metadata.md#7.6%20Decisions%20are%20observable),
[RFC 17 §3.3](rfc-17-vfs.md#3.3%20Event%20hooks)) for a decision an audit entry
**selects**, when the share's audit policy honours file entries. An effective
audit or alarm entry selects a decision when:

- it names the caller, matched as the walk matches (§2.3);
- its mask holds at least one right the request asked for, after generic
  mapping;
- it carries the successful-access flag and the decision granted, or the
  failed-access flag and the decision refused.

One decision produces at most one event, however many entries select it. A
privileged caller's decisions are selected like anyone's. Audit entries are
inherited like access entries (§4.4), so an entry on a share's root reaches every
file created below it after it was set.

The share's audit policy ([RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings))
decides whether file entries are honoured, beside its own selection of every
decision, denials only, or writes and namespace changes. With the policy off,
no entry selects anything and no event is built, as on Windows, where an audit
entry has effect only while object-access auditing is on.

## 4. Mode and ACL

![Mode and ACL as two views of one decision: on a read of a file with no stored ACL, the ACL the mode implies; on every set of an ACL, the mode it implies written in the same transaction; a chmod merges into the ACL by RFC 7's rule; setuid, setgid and sticky live only in the mode; the two round-trip identities hold](img/rfc19-mode-acl.svg)

RFC 7 requires the two to agree after every transaction and gives the `chmod`
merge ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)).
This section defines the two functions RFC 7 names but does not spell out —
`ACLFromMode` and `ACL.Mode` — and how inheritance is computed.

### 4.1 The ACL a mode implies

`ACLFromMode(m)` is the ACL a file with no stored ACL is evaluated against and
shown as. Let `u`, `g`, `o` be the owner, group and other bits of `m`, and
`R(bits)` the rights those bits grant
([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode),
"what a mode bit grants": any bit grants the minor rights; read adds read data,
write adds write data, append, write attributes and write extended attributes —
and on a directory add a file, add a subdirectory and delete a child — execute
adds execute). It is, in this order, with every entry whose mask is empty left
out:

1. deny `OWNER@` `R(g|o)` minus `R(u)`;
2. allow `OWNER@` `R(u)`;
3. deny `GROUP@` `R(o)` minus `R(g)`;
4. allow `GROUP@` `R(g)`;
5. allow `EVERYONE@` `R(o)`.

None is inheritable, and the list carries the defaulted flag. This is exactly
POSIX's class rule: an owner gets the owner bits and nothing a group or other
class adds; a group member who is not the owner gets the group bits; anyone else
the other bits. For every mode whose classes only narrow — owner ⊇ group ⊇
other, as 0755, 0750, 0644, 0640 and 0600 — no deny entry is produced, and a
Windows Security tab shows three allow entries and no deny.

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
it is shown to another server as the file's real permissions, and an operator
reading the Security tab is told a rule nothing enforces.

### 4.2 The mode an ACL implies

`ACL.Mode()` gives the nine permission bits an ACL implies; setuid, setgid and
the sticky bit are not in an ACL and are kept from the stored `Mode`. Each class
is the bits of what the walk (§3.3) would grant a hypothetical caller, mapped
back — read from read data, write from write data, execute from execute:

- **owner class**: a caller who is the file's owner, in no group, matched by
  `OWNER@`, `EVERYONE@` and entries naming the owner's principal;
- **group class**: the union, over `GROUP@` and every entry naming a principal
  other than the owner, of what a caller matched by that principal and
  `EVERYONE@` alone is granted — the mask POSIX ACLs show in the group bits;
- **other class**: a caller matched by `EVERYONE@` alone.

Only effective entries count, and the owner's implicit rights do not: they are
not mode bits.

The group class is the union over named principals, not `GROUP@` alone, so that
RFC 7's merge — which limits every named allow to the new group bits — is an
identity on the ACL's own mode. A `cp -p` or `chmod` that sets the mode it just
read must grant afterwards exactly what was granted before. With the group class
read from `GROUP@` alone, `chmod $(stat -c %a f) f` on alice's directory of §8
would strip `builders` of read, write and execute.

Two identities **MUST** hold, and are checked (§11):

- `ACLFromMode(m).Mode() == m` for every nine-bit `m`;
- for every ACL `a`, `a.WithMode(a.Mode())` grants every caller exactly what `a`
  grants, where `WithMode` is RFC 7's merge.

### 4.3 Setting mode and ACL

- Setting an ACL stores it and stores `Mode` as `ACL.Mode()` with the special
  bits kept, in one transaction
  ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)).
- A `chmod` of a file with a stored ACL is RFC 7's merge. The merge limits only
  the rights of `R` (§4.1) that a missing bit withholds, never the minor rights
  every bit grants, and keeps every other right of an entry. Where the ACL has no
  effective `OWNER@`, `GROUP@` or `EVERYONE@` allow to rewrite, the merge adds
  one, after the explicit entries, so it cannot override an explicit deny. `Mode`
  becomes the merged ACL's `Mode()`, which equals the requested bits unless a
  deny naming the owner's own principal withholds some; the stored `Mode` then
  shows what is granted, never more.
- A `chmod` of a file with no stored ACL changes `Mode` and stores no ACL.
- A request that sets both — an NFSv4 `SETATTR` naming `mode` and `acl`, or
  `mode` and `dacl` — stores the ACL, and takes from `mode` only setuid, setgid
  and sticky (§7.4).
- A `chown` or `chgrp` changes no entry. `OWNER@` and `GROUP@` follow it (§2.3).

### 4.4 Inheritance

**At create.** RFC 7 fixes when a new file inherits and how CREATOR OWNER,
CREATOR GROUP, the create mode and the umask apply
([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)).
The inherited list is computed from the parent's lists, each entry in stored
order, as [RFC 8881 §6.4.3](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4.3)
and MS-DTYP 2.5.3.4 compute it:

| Parent entry's flags | New file gets | New directory gets |
| --- | --- | --- |
| neither inherit flag | nothing | nothing |
| file inherit | an effective copy, no inherit flags | an inherit-only copy keeping file inherit, unless no propagate |
| directory inherit | nothing | an effective copy keeping directory inherit, unless no propagate |
| both | an effective copy, no inherit flags | an effective copy keeping both, unless no propagate |
| no propagate, with either | as above, no inherit flags | an effective copy with no inherit flags, where a directory gets one |

Every copy carries the inherited flag. An effective copy has its generic bits
mapped for the new file's type; an inherit-only copy keeps them as written. The
new list carries the auto-inherited flag when the parent's does. A parent's
protected flag does not pass to children; a list with the protected flag set
inherits nothing.

**At set.** A set of a list whose flags ask for inheritance to be computed (SMB's
`SE_DACL_AUTO_INHERIT_REQ`, `SE_SACL_AUTO_INHERIT_REQ`) recomputes that one
file's inherited entries from its parent's current lists, keeps the explicit
entries the client sent, and stores explicit before inherited. A set without
that request stores the entries as sent, inherited flags included.

**Never to descendants.** A change to a directory's ACL changes no other file.
Propagating it down a tree is the client's: Windows tools walk the tree and set
each file with the auto-inherit request, and NFSv4 has no propagation. A
server-side walk of an unbounded subtree inside one call would hold the
directory's transaction for as long as the tree is large.

> ponytail: no server-side propagation. Clients that propagate already walk the
> tree themselves, one set per file. Add an asynchronous, resumable propagation
> job — the shape of RFC 26's batched work — when an operator needs to change
> the permissions of a tree too large for a client to walk.

## 5. The shared translation

### 5.1 One translation, two spellings

Both adapters spell the ACL through **one translation**, a set of pure
functions over neutral types that both adapters import
([RFC 17 §4.2](rfc-17-vfs.md#4.2%20Translation%20stays%20in%20adapters),
[RFC 20 §2.3](rfc-20-adapters.md#2.3%20What%20an%20adapter%20may%20import)):

```go
// Spelling resolves principals for the translation. It is RFC 18's mapping,
// passed in; the translation does no lookup of its own.
type Spelling interface {
	SID(p metadata.Principal) (SID, error)              // every principal has one
	PrincipalBySID(s SID) (metadata.Principal, error)   // RFC 18's Find: ErrUnknownPrincipal for a SID no principal holds
	Who(p metadata.Principal) (string, error)           // NFSv4 who: OWNER@…, name@domain, or SID@domain
	PrincipalByWho(who string, group bool) (metadata.Principal, error)
}

func DescriptorFromACL(a metadata.ACL, owner, group metadata.Principal, parts SecurityInfo, sp Spelling) ([]byte, error)
func ACLFromDescriptor(sd []byte, current metadata.ACL, f metadata.File, sp Spelling) (Descriptor, error) // §6
func NFSv4FromACL(a metadata.ACL, attr NFSv4ACLAttr, admin bool, sp Spelling) (NFSv4ACL, error)
func ACLFromNFSv4(v NFSv4ACL, attr NFSv4ACLAttr, current metadata.ACL, admin bool, sp Spelling) (metadata.ACL, error) // §7
```

The translation **MUST**:

- change principals' spellings, flag bit positions and list framing, and
  nothing else: the kinds, the rights, the order and the flags' meanings cross
  unchanged;
- refuse, with `ErrInvalid`, anything §2.8 refuses, rather than store less than
  it was given;
- round-trip: for every ACL `a` the model holds, reading `a` through one protocol
  and writing back what was read stores `a`, and so does the same through the
  other protocol, for a caller allowed to see both lists.

The translation decides nothing about access. Whether the caller may read or
write the ACL is the chokepoint's, asked with the rights §6.1 and §7.1 name.

### 5.2 Principals on the wire

[RFC 18](rfc-18-identity.md) **MUST** give every principal a SID and an NFSv4
`who`, so that no entry is ever unprintable in either protocol. The translation
spells:

| Principal | SMB SID | NFSv4 `who` |
| --- | --- | --- |
| `OWNER@`, `GROUP@` | the file's owner's or group's SID, or CREATOR OWNER and CREATOR GROUP (§6.3) | `OWNER@`, `GROUP@` |
| `EVERYONE@` | `S-1-1-0` | `EVERYONE@` |
| Authenticated Users, NETWORK | `S-1-5-11`, `S-1-5-2` | `AUTHENTICATED@`, `NETWORK@` |
| owner rights, BUILTIN\Users, Administrators, SYSTEM, CREATOR OWNER, CREATOR GROUP | their fixed SIDs ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)) | the SID's string form at the ID-mapping domain, `S-1-3-4@example.com` |
| a user or group | from RFC 18 | RFC 18's owner-string spelling for the call's flavour — `name@domain` on Kerberos, the decimal uid or gid on `AUTH_SYS` — with the group flag for a group |

On a set, a `who` of the form `S-…@domain` with a well-formed SID resolves
through `PrincipalBySID`; any other `who` through `PrincipalByWho`, by RFC 18's
rules for owner strings, in the user or group namespace as the group flag says. NFSv4's other special names —
`INTERACTIVE@`, `BATCH@`, `DIALUP@`, `ANONYMOUS@`, `SERVICE@` — are well-known
principals with no row in RFC 7's table: an allow naming one is stored and
grants nothing, and a deny naming one is refused (§2.8).

**A deleted principal** has no spelling left
([RFC 18](rfc-18-identity.md) shows it as the null SID `S-1-0-0`); its entries
match no caller (§3.3). They are shown with that spelling, and on a set an entry
naming the null SID, or NFSv4's spelling of it, is left out and counted. This is
the one entry a set may leave out: it matched no caller before the set and
cannot match one after it, so no request is answered differently.

> decision: a principal NFS cannot name is spelled by its SID rather than left
> out. Leaving it out makes every read-modify-write from an NFS client a silent
> delete of that entry, a deny included, which is the failure of
> [Start here](#Start%20here). A `who` that is a SID is legal UTF-8 an NFS tool
> passes through unread. Overturned if a widely used NFS client is shown to
> refuse or mangle such a `who`; the fallback then is to refuse the NFS ACL
> write of a file holding an unspellable entry, never to drop it.

## 6. SMB security descriptors

### 6.1 The parts

| Part | Model | To read | To write |
| --- | --- | --- | --- |
| owner SID | the File's `Owner` | read the ACL | change the owner, under [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved)'s rules for the value |
| group SID | the File's `Group` | read the ACL | change the owner, under RFC 7 §7.4 |
| DACL | the access list and its flags | read the ACL | write the ACL |
| SACL | the audit list and its flags | system security (§3.5) | system security |

The rights are asked of the chokepoint through the open the request came on
([RFC 17 §4.6](rfc-17-vfs.md#4.6%20One%20chokepoint)), so the open's grant
decides, as Windows' set-security does. A query returns only the parts its
security-information bits ask for. A set changes only the parts it names, and
the parts it names change together, in one transaction, with `Mode` (§4.3).

An owner or group SID on a set resolves through RFC 18 like any SID. One that
cannot be resolved is refused `ErrInvalid`; it is never ignored with the rest of
the set applied.

A descriptor sent with a `CREATE` (the `SecD` context) is the new file's ACL,
owner and group, set under the same rules in the create's transaction, in place
of the inherited ACL.

The descriptor's own flags map to §2.6's. `SE_DACL_PRESENT` with no DACL is the
null DACL of §2.6. The protected and unprotected security-information bits
(`PROTECTED_DACL_SECURITY_INFORMATION` and its siblings) set or clear the
protected flag of the list they name.

### 6.2 Entries

- Allow, deny, audit and alarm entries of the basic forms map to §2.2's kinds.
  The DACL holds allow and deny; the SACL holds audit and alarm. An audit entry in
  a DACL, or an allow in a SACL, is refused.
- Masks are entry rights after generic mapping and clearing (§2.4).
- Flags map by name (§2.5).
- Order is kept (§2.7). An explicit entry after an inherited one is stored where
  it was sent.

### 6.3 Owner and group entries become `OWNER@` and `GROUP@`

Windows has no `OWNER@`: an entry for the file's owner is an entry for the
owner's SID, and an entry meant for whoever creates a child is CREATOR OWNER,
inherit-only. The translation keeps `OWNER@` and a named owner entry apart this
way:

- **Read.** An effective `OWNER@` entry with no inherit flags is spelled with the
  owner's SID. An inherit-only `OWNER@` entry is spelled CREATOR OWNER. An
  `OWNER@` entry both effective and inheritable is spelled as two: the owner's
  SID with no inherit flags, then CREATOR OWNER inherit-only with the inherit
  flags. `GROUP@` likewise, with the group's SID and CREATOR GROUP.
- **Write.** An entry with no inherit flags naming the SID that will be the
  file's owner after the set is stored as `OWNER@`, and one naming the group's
  SID as `GROUP@`. Every other entry is stored with the principal it names.

So the read and the write are inverse: the Security tab shows alice's own SID for
the `OWNER@` entry of a mode-derived ACL, and writing the tab back keeps it
`OWNER@`, which RFC 7's `chmod` merge rewrites from the owner bits rather than
limiting as a named entry.

> decision: an effective entry naming the owner's SID is stored as `OWNER@`.
> Without it, every Explorer edit of a file with a mode-derived ACL turns the
> owner's entry into a named entry for alice, which a later `chmod 750` limits to
> the group bits and a `chown` leaves granting alice. The cost: an entry a
> Windows user meant for alice personally follows the file to its next owner.
> Overturned if a workload is shown to `chown` files whose Windows ACLs name the
> old owner and expect that owner to keep access.

### 6.4 What is not supported

| Descriptor content | Answer |
| --- | --- |
| object entries (`ACCESS_ALLOWED_OBJECT_ACE` and its siblings) | the set is refused `ErrInvalid` |
| callback (conditional) entries, allow, deny or audit | refused `ErrInvalid` |
| mandatory-label, resource-attribute and scoped-policy entries, and the label, attribute and scope security-information parts | a query returns none; a set carrying any is refused `ErrInvalid` |
| an entry kind or revision the model does not hold | refused `ErrInvalid` |
| more than 128 entries in a list, or a list over 64 KiB | refused `ErrInvalid` |
| a null DACL | stored as allow `EVERYONE@` every right, and read back so (§2.6) |

The status code each refusal is answered with is RFC 22's.

> decision: an unsupported entry fails the whole set rather than being skipped.
> Skipping a callback deny grants what it denied; skipping an allow quietly
> withdraws a grant. Both look like success to the client. Overturned only by
> supporting the kind; a tool that sets one is told so, and can fall back.

## 7. NFSv4 ACLs

### 7.1 `acl`, `dacl`, `sacl` and `aclsupport`

| Attribute | Versions | Holds | To read | To write |
| --- | --- | --- | --- | --- |
| `acl` | 4.0, 4.1, 4.2 | the access list, then the audit list; no flags | read the ACL, and system security for the audit entries | write the ACL, and system security for the audit entries |
| `dacl` | 4.1, 4.2 | the access list and its flags | read the ACL | write the ACL |
| `sacl` | 4.1, 4.2 | the audit list and its flags | system security | system security |
| `aclsupport` | all | allow, deny, audit and alarm | — | — |

- A `GETATTR` of `acl` by a caller without the system-security right returns the
  access list alone; a `SETATTR` of `acl` by such a caller that holds an audit or
  alarm entry is refused, and one that holds none leaves the stored audit list
  as it was ([RFC 21 §8.1](rfc-21-nfs.md#8.1%20Supported)).
- A `SETATTR` of `acl` leaves both lists' flags as stored: `acl` carries none.
- A `SETATTR` naming `acl` together with `dacl` or `sacl` is refused
  `ErrInvalid`: two values for one list.

### 7.2 Entries and `who`

- `ACE4_ACCESS_ALLOWED_ACE_TYPE`, `ACE4_ACCESS_DENIED_ACE_TYPE`,
  `ACE4_SYSTEM_AUDIT_ACE_TYPE` and `ACE4_SYSTEM_ALARM_ACE_TYPE` are §2.2's
  kinds; any other type is refused.
- The group flag (`ACE4_IDENTIFIER_GROUP`) is set on read exactly when the
  principal is a group or `GROUP@`, and on write selects the namespace a `who`
  is resolved in (§5.2). It is not stored.
- Masks and flags as §2.4 and §2.5; order as §2.7.
- A `who` that resolves to no principal is refused `ErrInvalid`, which RFC 21
  answers `NFS4ERR_BADOWNER`.

### 7.3 `ACCESS` bits

NFSv3 and NFSv4 `ACCESS` report the bits whose rights are in `G` (§3.4); the
bits have the same values in both versions.

| `ACCESS` bit | On a file | On a directory |
| --- | --- | --- |
| `READ` | read data | list |
| `LOOKUP` | — | traverse |
| `MODIFY` | write data | delete a child |
| `EXTEND` | append, or write data | add a file, or add a subdirectory |
| `DELETE` | — | delete a child |
| `EXECUTE` | execute | — |
| `XAREAD`, `XALIST` | read extended attributes | read extended attributes |
| `XAWRITE` | write extended attributes | write extended attributes |

A bit with no right on that file type is never reported.

### 7.4 `mode` and `acl` in one `SETATTR`

A `SETATTR` naming `mode` and `acl` (or `dacl`) stores the ACL, takes setuid,
setgid and sticky from `mode`, and stores `Mode` as the ACL's `Mode()` with those
bits (§4.3). Its permission bits are not merged: the client sent the ACL it
wants, and merging a mode into it would change it. `mode_set_masked` changes only
the bits its mask names, as a `chmod` of those bits.

## 8. A worked example across protocols

alice, bob and the build host's principal `build` are users of the `example.com`
domain; `build` and bob are members of `builders`. The `builds` share is
exported over NFS and SMB. alice's, bob's and `builders`' grant on `builds` is
`read-write`. `release` is a directory alice owns, group `builders`, with no
stored ACL and mode 0775.

**1. alice opens the Security tab from alice-pc.** Explorer queries owner,
group and DACL. alice owns the file, so her implicit right to read the ACL
answers it (§3.1 step 5). The directory has no stored ACL, so she is shown
`ACLFromMode(0775)` (§4.1), spelled with SIDs (§6.3):

| # | Kind | Principal shown | Rights | Flags |
| --- | --- | --- | --- | --- |
| 1 | allow | alice's SID (stored `OWNER@`) | `R(rwx)` | none |
| 2 | allow | `builders`' SID (stored `GROUP@`) | `R(rwx)` | none |
| 3 | allow | Everyone | `R(r-x)` | none |

**2. alice sets a new DACL.** She replaces the three with these, which Explorer
writes in canonical order, with the protected flag:

| # | Kind | Principal | Mask | Flags |
| --- | --- | --- | --- | --- |
| 1 | deny | bob | `0x116`: write data, append, write extended attributes, write attributes | `OI CI` |
| 2 | allow | alice | `0x1F01FF`: full control | `OI CI` |
| 3 | allow | `builders` | `0x1301BF`: modify | `OI CI` |

The set asks for write the ACL, which alice holds implicitly as owner, within
her read-write ceiling. The translation resolves the three SIDs to principals,
maps the flags by name, and stores the entries and the flag in this order
(§6.2). Entry 2 names the owner's SID with inherit flags, so it is stored
as a named entry for alice, not as `OWNER@` (§6.3). `Mode` becomes the ACL's
`Mode()` in the same transaction (§4.2): the owner class from entry 2 is `rwx`;
the group class is the union of what `builders` (`rwx`) and bob (nothing: his
only entry is a deny) are granted, `rwx`; the other class is empty. `Mode` is
0770.

**3. build01 reads it.** `nfs4_getfacl /mnt/builds/release`, on a Kerberos mount, sends
`GETATTR` of `acl` as `build`. Reading the ACL is in `builders`' modify, so the walk grants it
(§3.3). `build` holds no system-security right, so no audit entry is returned,
and there is none. The translation spells (§5.2, §7.2):

```text
D:fd:bob@example.com:waNT
A:fd:alice@example.com:rwaDdxtTnNcCoy
A:fdg:builders@example.com:rwadxtTnNcy
```

`ls -l` shows `drwxrwx---`, the stored `Mode`.

**4. A new file inherits.** build01 creates `release/notes.txt` with mode 0664.
The parent's entries all carry file inherit, so the file gets the three as
effective copies with no inherit flags and the inherited flag; the create mode
is an **and** that limits the named allows to its group bits, `rw-`, so alice
and `builders` keep every inherited right but execute, and adds no entry
(RFC 7 §2.6). The file's `Mode` is the inherited ACL's `Mode()`, 0660.

**5. bob is refused over both protocols.** bob opens `release/notes.txt` from
his Windows desktop asking `GENERIC_WRITE`. The request maps to
`FILE_GENERIC_WRITE`, `0x120116`. His ceiling is every right; he is neither
privileged nor the owner. The walk meets entry 1 first: it names bob and covers
write data, append, write extended attributes and write attributes, none yet
decided, so they are refused. Entry 3 matches through `builders` and grants read
the ACL and synchronize, but not the four already refused. The request is
refused, `STATUS_ACCESS_DENIED`. From his Linux laptop over NFSv4, the `OPEN`
for write runs the same walk and is refused, `NFS4ERR_ACCESS`, and an `ACCESS`
asking `MODIFY` and `EXTEND` reports neither. A read succeeds over both.

**6. build01 writes, and cannot re-permission.** `build` writes
`notes.txt`: entry 3 grants write data. `nfs4_setfacl -a` on it asks write the
ACL, which modify does not hold; it is refused. Had `build` been granted it, the
tool would read the three entries, append one, and write four back, bob's deny
first and intact.

**7. alice runs `chmod 750 release` from a Linux box.** RFC 7's merge applies
(§4.3): the named allows for alice and `builders` are each split into an
inherit-only copy, unchanged, and an effective copy limited to the new group
bits, `r-x`; bob's deny is untouched; allow `OWNER@` `R(rwx)` and allow
`GROUP@` `R(r-x)` are added after the explicit entries. `Mode()` of the result
is 0750: the owner class is `OWNER@`'s `rwx`, the group class the union of
`builders`' and `GROUP@`'s `r-x`. alice keeps write on `release` through
`OWNER@`; `builders` can no longer add files to `release` itself, and every file
created below it still inherits modify from the unchanged inherit-only copy. A
`chmod` narrows a named entry for the owner as it narrows any named entry, which
is what POSIX ACLs do; §6.3's rule keeps the entries a Windows client writes
without inherit flags out of that case.

## 9. Invariants

| # | Invariant |
| --- | --- |
| Z1 | A stored entry names its principal by ID, holds only the fourteen entry rights, and is one of four kinds. |
| Z2 | A set either stores every entry it carried, in its order — entries naming deleted principals aside — or stores nothing. |
| Z3 | The same request by the same identity of the same file is granted the same set, whatever protocol carried it. |
| Z4 | `Authorize(want)` succeeds exactly when `want` is within `Granted`, and `Granted` is within the share ceiling. |
| Z5 | No file ACL widens a share ceiling, and no share ceiling is written into a file's ACL, stored or shown. |
| Z6 | The ACL a client is shown is the ACL the walk reads, `ACLFromMode` of the mode where none is stored. |
| Z7 | `ACLFromMode(m).Mode() == m`, and `a.WithMode(a.Mode())` grants what `a` grants. |
| Z8 | Reading an ACL through either protocol and writing back what was read stores the same ACL, less entries naming deleted principals. |
| Z9 | Every principal has a SID and an NFSv4 `who`; no entry is left out of a read for want of a spelling. |
| Z10 | Audit and alarm entries never change a decision; the system-security right is granted only to an identity with `admin` set. |
| Z11 | A change to one file's ACL changes no other file's. |

## 10. Observability

Metric names are shown without the deployment's prefix. Decision counts and
latency are the filesystem service's; events dropped are RFC 7's.

| Answers | Metric | Type |
| --- | --- | --- |
| ACL sets refused, and why (`kind`, `principal`, `limit`, `deny_unmatched`) | `acl_set_refused_total{protocol, reason}` | counter |
| mask or flag bits cleared on a set (§2.4, §2.5) | `acl_bits_cleared_total{protocol, field}` | counter |
| reads spelled with a SID fallback `who` (§5.2) | `acl_who_sid_fallback_total` | counter |
| decisions refused by the share ceiling and not by the file | `acl_ceiling_refusals_total` | counter |
| access events selected by audit entries | `acl_audit_selected_total{outcome}` | counter |

`acl_bits_cleared_total` and `acl_who_sid_fallback_total` are not errors; a
sudden rise names a client sending what the model does not hold, or principals
RFC 18 cannot name. No metric carries a share or principal label
([RFC 7 §11.2](rfc-7-namespace-metadata.md#11.2%20Observability)).

## 11. Conformance

Tiers are the harness's ([index](rfc-index.md#Test%20tiers)). Each row runs
through both adapters on the composed node unless it names one, so the check
sits at the consumer of the translation, not beside it.

| § | Check |
| --- | --- |
| §4.2 the two identities | For every nine-bit mode `m`, assert `ACLFromMode(m).Mode() == m`. For a generated corpus of ACLs — named users and groups, denies before and after allows, inherit-only and both-flag entries, owner rights — assert `a.WithMode(a.Mode())` grants every caller of a fixed population exactly what `a` grants. A `Mode()` reading the group class from `GROUP@` alone fails the second on the first ACL holding a named allow. |
| §4.1 shown is evaluated | On a 0640 file with no stored ACL, query the descriptor over SMB and `acl` over NFSv4: assert both are `ACLFromMode(0640)`, and that each listed principal is granted exactly what its entry says. On 0604: assert a group deny is shown and a group member is refused read over both. |
| §2.8, §6.4 no partial set | Set over SMB a DACL holding a callback deny for bob and an allow for everyone; assert the set is refused and the stored ACL unchanged, and bob's read still refused or granted as before. Repeat with an object allow, and with 129 entries. |
| §5.2, Z8 round trip | Set from SMB an ACL naming a SID-only group with a deny, owner rights, CREATOR OWNER inherit-only, and a user with an NFS name. Read it over NFSv4 as an administrator, write back exactly what was read, read it over SMB: assert byte-equal descriptors. Then the reverse, starting from `nfs4_setfacl`. A translation that leaves out an entry it cannot name fails on the deny. |
| §6.3 owner entries | On a mode-0755 file owned by alice, read the descriptor over SMB, write it back unchanged, `chown` the file to bob: assert bob, not alice, holds the owner's rights. Write a descriptor naming alice's SID with `OI CI` on a directory: assert it stays a named entry, and a new child owned by bob does not grant bob alice's rights. |
| §3.2 ceiling | bob's grant `read`; a file's ACL allows him full control: assert `Granted` over both protocols is the read row, a write open is refused, and `MAXIMUM_ALLOWED` is granted read. As the file's owner on that share, set the ACL: assert refused. As a privileged caller on a read-only admission: assert no write. |
| §3.4 maximum allowed | `MAXIMUM_ALLOWED` alone where nothing is granted: refused. `MAXIMUM_ALLOWED` with `FILE_WRITE_DATA` where only read is granted: refused. With nothing explicit and read granted: granted read. Run the same descriptors against a Windows server and assert the same answers. |
| §3.7 audit selection | With the share's policy honouring file entries and an inheritable failed-write audit entry on the root: a refused write by bob over SMB and over NFS each emit one event; a granted write emits none; with the policy off, none. A privileged caller's refused execute emits one. |
| §3.5 system security | As a file's owner without `admin`: SMB query of the SACL is `STATUS_PRIVILEGE_NOT_HELD`, NFS `sacl` is refused. As an `admin` over AUTH_SYS, where `admin` is never set: refused. As an authenticated `admin`: both succeed. |
| §7.1, §7.4 attributes | `SETATTR` of `acl` and `dacl` together: refused. `SETATTR` of `mode` 04750 with an `acl`: assert the ACL stored as sent, setuid set, and `Mode` the ACL's own bits. |
| §4.4 inheritance | Each row of §4.4's table, over both protocols and against a Windows server. A set with the auto-inherit request on a child whose parent changed: assert the child's inherited entries recomputed and its explicit ones kept; assert its siblings unchanged. |
| §2.3 kinds | A user entry for a principal whose UID equals a group's GID: assert a member of that group is not matched. |

**What must not stand in.** A round trip through the translation alone, without
the store between the write and the read, is not evidence for Z8: it proves only
that the two halves agree with each other. A check against the mode-only path is
not evidence for the ACL path, and the reverse.

## 12. Open questions

1. **Maximum allowed with generic bits.** Samba grants a `MAXIMUM_ALLOWED`
   open naming `GENERIC_READ` or `GENERIC_EXECUTE` even when not every right
   they map to is granted. Whether Windows does, which §3.4's rule decides
   against, is to be settled by the §11 run against a Windows server.
2. **A `who` that is a SID.** §5.2's spelling is untested against the NFSv4
   ACL tools of every client the reference profile lists; the decision note
   names the fallback.
3. **Audit list order and flags in `acl`.** NFSv4.0 carries both lists in one
   attribute with no flags; whether a client that interleaves audit and access
   entries expects their relative order back is unmeasured. The model drops it.
4. **Share ceiling levels.** Whether a fifth level — write without
   re-permissioning — is needed is an operator question (§3.2).
5. **Mandatory labels.** Whether any Windows workload on the reference profile
   sets a mandatory-label entry on files, which §6.4 refuses, is unmeasured.

## Appendix A — prior art

*Non-normative.*

- **Windows' access check** (MS-DTYP 2.5.3.2) — the owner's implicit rights
  before the DACL, owner rights, the ordered allow and deny walk, `MAXIMUM_ALLOWED`;
  the generic mapping for files (MS-FSA 2.1.5.1.2); inheritance (MS-DTYP
  2.5.3.4) and propagation done by the caller walking the tree.
- **NFSv4 ACLs** ([RFC 8881 §6](https://www.rfc-editor.org/rfc/rfc8881.html#section-6))
  — the shared mask bits, the `acl`, `dacl` and `sacl` attributes, the mode and
  ACL interaction of §6.4, inheritance of §6.4.3.
- **POSIX ACLs** — the mask entry shown in a file's group bits, which §4.2's
  group class follows, and the rule that `chmod` of the group bits sets the mask.
- **Samba's NFSv4 ACL mapping** — maps an entry for the owner's SID to the NFSv4
  owner special principal under some of its settings, the model for §6.3; its
  share ACLs are a ceiling intersected with the file ACL, the model for §3.2.
- **The Linux NFS server** — translates NFSv4 ACLs to POSIX ACLs and refuses
  what POSIX cannot hold, rather than storing less: the model for §2.8.
- **ZFS and its NFSv4 ACLs** — one ACL model served to both NFS and SMB, with
  a property choosing how `chmod` treats an ACL; the reason §4 fixes one rule
  rather than a setting.

## Appendix B — where the current code differs

*Non-normative.* The code on the branch this RFC was written against, by section.

| § | This RFC | The code today |
| --- | --- | --- |
| §2.3 | principals by ID, kind part of the entry | an entry's principal is a string: `OWNER@`-style specials, `sid:<SID>`, `name@domain`, or `<number>@localdomain`; the last matches the caller's UID **or** any of its GIDs, so a user entry for uid 1000 matches every member of gid 1000 |
| §2.3, §3.3 | owner rights leaves `OWNER@` matching | an effective owner-rights entry stops `OWNER@` entries matching the owner, as Samba does |
| §2.6 | null DACL stored as one allow-everyone entry | stored as a flag the evaluator checks first |
| §2.8, §6.4 | an unsupported entry refuses the set | the descriptor parser skips entry types it cannot map, object and callback denies included, and stores the rest |
| §3.1 step 5 | the owner's implicit rights are not removable by a deny | an explicit deny removes them per bit; an administrator SID owner also receives change-the-owner implicitly, as a take-ownership privilege |
| §3.1, RFC 7 §7.1 | the share ceiling is applied at the chokepoint | the SMB write and create handlers test the session's share permission themselves; the metadata layer separately strips write and delete for a read-only grant, and a handle-based write bypass skips the file check unless the ACL holds a deny |
| §3.2 | the ceiling is never written into an ACL | share grants are synthesised into the share root's ACL, and direct SID grants are added to the descriptor shown for files with no stored ACL, display only |
| §3.4 | maximum allowed as Windows' access check | `GENERIC_READ` and `GENERIC_EXECUTE` beside `MAXIMUM_ALLOWED` are granted best-effort, as Samba does |
| §3.7 | audit entries select events | audit entries are stored and shown and select nothing |
| §4.1 | the mode-implied ACL is shown and evaluated | a file with no stored ACL is shown owner and SYSTEM full control over SMB and an empty `acl` over NFSv4, and evaluated by a separate mode check in which the write bit grants delete on the file and ownership grants change-the-owner |
| §4.2, RFC 7 §2.6 | setting an ACL stores the mode it implies | setting an ACL leaves `Mode` unchanged; the mode-from-ACL function has no production caller |
| §4.3, RFC 7 §2.6 | `chmod` limits named allows | `chmod` rewrites only the owner, group and everyone entries and leaves named entries unchanged |
| §4.4 | no server-side propagation | a propagation function exists with no production caller, and the SMB fidelity guide lists recursive propagation as working |
| §5.1, RFC 20 §2.3 | one translation both adapters share | NFS writes stored who strings verbatim; SMB has its own principal-to-SID mapping inside its handler package |
| §6.1 | an unresolvable owner SID refuses the set | it is ignored and the rest of the set applied |
| §7.1 | `acl`, `dacl`, `sacl` | only `acl` is served; it carries no audit entries on read, and audit entries sent in it are stored in the access list; `aclsupport` reports all four kinds |
| §7.2 | the group flag from the principal's kind | the group flag is neither set on read nor read on write |
| §7.3 | `ACCESS` bits by right | a directory's `MODIFY` and `EXTEND` both map to one write permission needing write data and append together |
| docs | — | the ACL design document still states that ACLs are validated for canonical order, which the code no longer does |
