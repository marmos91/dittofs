---
rfc: 18
title: "RFC 18 — identity and authentication"
component: identity
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-19-authorization]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-21-nfs]]"
  - "[[rfc-22-smb]]"
  - "[[rfc-23-control-plane]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-25-observability]]"
  - "[[rfc-26-catalog-backups]]"
aliases:
  - RFC 18
  - identity and authentication
tags:
  - rfc
---
# RFC 18 — identity and authentication

**Status:** draft. [§12](#12.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone changing how a client signs in, how a user, group or
Windows SID becomes a principal, how squashing works, or any cache keyed on who
the caller is.

| This RFC | Not this RFC |
| --- | --- |
| **Who** the caller is: principals and their kinds, the well-known principals (name, ID, who matches), spellings, sign-in and what it proves, and the caller a call acts as on one share — its principal, groups, deny-only principals, share access and its `authenticated`, `admin` and privileged flags, after squashing | **What that caller may do to a file** — entries, rights, the order a decision runs in, the share ceiling, what privilege lets a caller do: [RFC 19](rfc-19-authorization.md). The records and their key encodings: [RFC 16](rfc-16-metadata-store.md#2.2%20Identity%20types). How each protocol spells a principal, a credential or a refusal on the wire: [RFC 21 §11](rfc-21-nfs.md#11.%20Translation), [RFC 22 §16](rfc-22-smb.md#16.%20Translation) |

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix B](#Appendix%20B%20%E2%80%94%20where%20the%20current%20code%20differs) says where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

Every call a client makes is made by someone. This RFC says who, and how we
know, in three steps.

1. **Sign-in.** Where a protocol proves something — a Kerberos ticket, an NTLM
   exchange — the adapter passes the handshake to `vfs.Service`'s `SignIn`, and
   identity checks it (§3). The adapter keeps the result for the connection. A
   claimed or empty credential — NFS `AUTH_SYS`, `AUTH_NONE` — signs in to
   nothing: the adapter goes straight to step 2 with what the client stated.
2. **Mapping.** What was proved or claimed — a Kerberos name, a SID, a uid —
   names a **principal**: the never-reused ID that files, ACLs, grants and
   quotas name (§2). Mapping also runs the other way, so a stored principal can
   be shown as a uid, a SID or an `owner@domain` string (§4).
3. **Resolution per share.** On every call, the share's policy turns that into
   the **caller**: squashing may replace the principal with the share's
   anonymous one, the groups are collected, the share's grants give its access,
   and three flags say how far it is trusted (§6).

What the caller may then do to a file is [RFC 19](rfc-19-authorization.md)'s.

**The problem, with one example.** alice and bob are users in the company's
directory. Neither has a Unix uid recorded there. A server that fills a missing
uid with a default — 1000, say — gives both of them uid 1000.

alice signs in on alice-pc over SMB. Her Kerberos ticket carries her Windows
groups, among them `Finance`. bob mounts `builds` from build01 with `sec=krb5`.
The NFS adapter, seeing uid 1000 for both, caches "uid 1000 on `builds`" with
the identity it built first: alice's, with `Finance`. bob's next call hits that
entry, and he now matches every ACL entry that grants `Finance`. Nothing errors
and no test goes red: every test used one user.

With this RFC:

1. alice and bob are two principals. A missing uid is never defaulted; each
   gets one allocated, recorded once and never reissued (§2.3).
2. The one cache of who a caller is keys on what was proved — the proof kind,
   the Kerberos name, the SIDs — never on a uid alone (§7.1). bob's call misses
   alice's entry.
3. The conformance suite signs alice in over Kerberos, sends her uid from
   another host over `AUTH_SYS`, and asserts that the second caller gets none
   of her groups (§10).

**The words you need.**

- **principal** — anything a file, ACL entry, grant or quota can name: a user, a
  group, a machine, or one of a few fixed identities such as anonymous (§2.1).
- **spelling** — one protocol's name for a principal: a uid, a SID, a Kerberos
  name, an `owner@domain` string. Nothing inside the service compares
  spellings; it compares principal IDs (§2.2).
- **proof kind and protection** — two separate, ordered measures of a request.
  The *proof kind* says how well the sender was checked: `None` (nothing was
  presented), `Claimed` (the client stated numbers nobody checked), `Secret` (it
  answered a challenge only a password holder could) or `Ticket` (a trusted
  third party vouched for it). *Protection* says what guards the messages:
  `None`, `Integrity` (signed) or `Privacy` (encrypted). An `AUTH_SYS` call is
  `Claimed`/`None`; the same user on `sec=krb5i` is `Ticket`/`Integrity`. A
  share sets a floor on each (§6.1).
- **evidence** — what one sign-in established: the verified name, the principal
  it was bound to, and the group SIDs a trusted party signed (§3.1).
- **caller** — what one call acts as on one share: a principal, its groups,
  its share access and three flags (§6.1).
- **squash** — replacing the caller's principal with the share's anonymous one
  (§6.2).

**What this RFC promises.**

- Two different proofs never collapse into one caller, whatever numeric IDs
  they share, and no cache can make them.
- An ID, once given to a principal, is never given to another.
- An outage of the store or the directory refuses what it cannot decide; it
  never grants a default, a guest's access or an anonymous one in its place.
- A failed sign-in is a failed sign-in, never a guest session.

The rest are invariants, [§9.1](#9.1%20Invariants).

![Sign-in on the node holding the connection: the adapter passes one handshake round through vfs.Service's SignIn to identity's Authenticator, which throttles first, then verifies the ticket or NTLMv2 response against the keytab, an NT hash or a domain controller and binds it to one principal. The adapter keeps the returned Identity, with the keys beside it, and answers any error as a logon failure, never a guest session](img/rfc18-signin.svg)

![One request: the gateway forwards the adapter's Identity unread to the file's primary, which fences it, admits it, resolves it to a Caller unless the one caller cache already holds it, and then asks Files.Authorize. Any store or directory error, disabled principal or over-cap group list is refused, never served as guest or anonymous](img/rfc18-resolution.svg)

### Scenarios

**alice signs in on alice-pc and opens her profile container.** alice-pc sends
`SESSION_SETUP` with a Kerberos ticket. The SMB adapter hands it to `SignIn`;
identity decrypts it with the keytab key, checks its times, verifies the PAC —
the group list the domain controller signed — checks that the PAC's user SID is
the SID on alice's record, and binds the evidence to alice's principal (§3.1,
§3.4). Her first `CREATE` on `profiles` carries an `Identity` built from that
evidence. Resolution re-reads alice's record — not disabled — maps her group
SIDs to group principals, finds nothing to squash and reads her grant,
read-write. The caller is alice, `authenticated`, not `admin`. One ticket, one
principal, on every call of the session.

**The domain controller stops answering during a sign-in storm.** At 08:55 the
directory stops answering. Users whose entries the node resolved within the last
20 minutes — the 5-minute TTL plus the 15-minute grace — are served from the
cache, counted as stale (§5.3). A user the node has never resolved, or resolved
too long ago, is refused with the protocol's logon failure, counted. Nobody is
let in as a guest. Known users keep working for a bounded time; nobody new gets
in until the directory answers.

| Scenario | Outcome | § |
| --- | --- | --- |
| build01's root runs `tar xpf` on a share with `Squash = root` | uid 0 maps to root, which is squashed to the share's anonymous principal; its `chown` is refused `EPERM`, as on a Linux server with `root_squash` | §6.2 |
| an operator disables bob while bob-pc holds open handles | the principals generation rises; within a second every node refuses bob's next call, on every protocol, though his handles stay open | §7.2 |
| bob lists `builds` over NFSv4 on Kerberos | owners show as `name@domain`; a deleted owner as `nobody@domain`; a name in another domain is refused | §4.5 |

---

## 1. Purpose

Identity decided in many places is identity with many gaps: each adapter maps
names its own way, each cache keys on the fields its author thought of, and
each outage path picks its own fallback. This RFC owns identity once.

### 1.1 Non-goals

This RFC **MUST NOT** be read as specifying any of these, which their owners
state:

| Topic | Owner |
| --- | --- |
| what a caller may do to a file: entries, rights, evaluation, the share ceiling, privilege's effect | [RFC 19](rfc-19-authorization.md) |
| the chokepoint, and the ACL as a record | [RFC 7 §7.1](rfc-7-namespace-metadata.md#7.1%20One%20chokepoint), [§2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode) |
| the records — `User`, `Group`, `Membership`, `ShareGrant`, `ExportPolicy`, `Secret`, the protocol-ID index — their store views and key encodings | [RFC 16](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities) |
| the order of admission: share state, minimums, client rules, then this RFC's resolution | [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees) |
| handshake wire forms, and which credential gives which proof kind and protection | [RFC 21 §11.1](rfc-21-nfs.md#11.1%20Identity), [RFC 22 §16.1](rfc-22-smb.md#16.1%20Identity) |
| the management API's tokens, sessions, roles and sign-in throttle | [RFC 23 §5](rfc-23-control-plane.md#5.%20Authentication%20and%20authorization) |
| where secrets are stored and how they rotate | [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets) |

OpenID Connect reaches the management API only: neither NFS nor SMB can carry
a bearer token, so an identity provider reaches file access only as a directory
or a Kerberos realm it backs (§5).

## 2. Principals

### 2.1 Kinds and well-known principals

A **principal** is a `Principal` value
([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)): a kind and a
`PrincipalID`, a random number drawn once and never reused. Principals differ in
who creates their record. An operator creates **local users and groups** through
the management API. Identity creates the others itself, the first time it meets
them; this RFC calls that **minting a record**. A **directory user or group** is
minted at its first lookup in the directory, and a **machine** at its first
Kerberos sign-in under a host or service name. **Well-known** principals are
never created: they have the same fixed ID in every installation. Minting a
record never grants file access.

*Example.* The operator creates alice, a local user. bob exists only in the
directory; at his first sign-in identity mints his record, with a fresh
`PrincipalID` and his directory SID. build01's NFS client signs in as
`host/build01.corp.example@CORP.EXAMPLE`, and identity mints a machine record
for it.

| Kind | Its record comes from | Spellings it carries | Can it sign in? |
| --- | --- | --- | --- |
| **local user** | the management API: a `User` record | name, uid, its primary group's gid, SID; optionally a Kerberos name | NTLMv2 against its NT hash; Kerberos if a KDC issues it tickets; `AUTH_SYS` by uid |
| **directory user** | minted at first lookup (§5.1), then a `User` record like any other | the directory's name and entry ID; SID and uid from the directory or allocated | Kerberos; NTLM pass-through; `AUTH_SYS` by uid |
| **group** | a `Group` record, local or minted from the directory | name, gid, SID | never |
| **machine** | minted at the first Kerberos sign-in of a host or service name | its Kerberos name only: no file spelling (§2.4) | Kerberos only |
| **well-known** | fixed IDs, below | as RFC 21 and RFC 22 spell them | anonymous and root only, as below |

**Well-known principals.** Each has a fixed ID, so a stored ACL entry can name
it; the byte is its ID's last byte
([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)). Each is
matched by a field of the caller (§6.1), never by privilege: a privileged caller
skips the walk ([RFC 19 §3.5](rfc-19-authorization.md#3.5%20Privilege%20and%20the%20system-security%20right)),
so a principal matched by privilege would grant nothing anyone walks.

| Byte | Principal | Who matches |
| --- | --- | --- |
| `0x01` | owner (`OWNER@`) | the file's current owner |
| `0x02` | group (`GROUP@`) | a member of the file's current group |
| `0x03` | everyone | every caller |
| `0x04` | owner rights | the file's owner; an entry naming it withdraws the owner's implicit rights ([RFC 19 §3.1](rfc-19-authorization.md#3.1%20One%20request%2C%20in%20order)) |
| `0x05`, `0x06` | creator owner, creator group | no caller: placeholders inheritance replaces ([RFC 19 §4.4](rfc-19-authorization.md#4.4%20Inheritance)) |
| `0x07` | authenticated users | a caller with `Authenticated` set (§6.5) |
| `0x08` | administrators | a caller with `Admin` set (§6.5) |
| `0x09` | system | no caller: nothing a client presents is the server's own system |
| `0x0A` | network | every caller: every caller arrives over the network |
| `0x0B` | built-in users | as authenticated users |
| `0x0C` | **anonymous** | a caller whose `Principal` is it: a `None` caller, an anonymous or guest session, a caller squashed under a policy whose `Anonymous` is it (§6.2) |
| `0x0D` | **root** | a caller whose `Principal` is it: a claimed uid 0, or a name an operator linked to it (§4.2) |

- **Anonymous** holds uid and gid 65534, reserved to it. It is never
  `authenticated`, never `admin` and never privileged.
- **Root** holds uid and gid 0, reserved to it. It is privileged where a share
  leaves it unsquashed (§6.5), and owns what it creates there, as the superuser
  does on a POSIX server.
- A caller whose `Authenticated` is unset matches neither authenticated users
  nor built-in users, though an entry naming its own principal does; what that
  means for a tree's ACL is
  [RFC 19 §2.3](rfc-19-authorization.md#2.3%20Principals)'s.
- A well-known principal with no row here matches no caller. What a set naming
  one does is [RFC 19 §2.8](rfc-19-authorization.md#2.8%20What%20a%20stored%20ACL%20may%20hold)'s.

> decision: uid 0 is a well-known principal, root, not a user an operator
> creates. Without it a claimed uid 0 maps to no principal, acts as anonymous,
> and a share with `Squash = none` behaves like `all` for root: `chown`,
> `tar xpf` and restores fail, and root's files are owned by 65534. The cost is
> that root is one principal across every client host, as it is to a POSIX
> server with `no_root_squash`. Overturned if a deployment needs root on one
> host told apart from root on another; that is a per-host link, not a second
> rule.

**Administrator is not a kind.** A principal is an administrator of a share
when that share's grant names it `admin`, and that counts only for a proof of
`Secret` or above (§6.5). No record, uid, group name or management-API role
makes a principal an administrator of files.

**Guest is not a kind either.** An SMB client that asks for a guest session
gets an anonymous one (§3.6).

![Creating a user: the management API validates the request and writes the record, its unique index rows, any IDs it names allocated and the raised principals generation in one create-only transaction. Every node then reads the new generation within a second and applies it, so the next sign-in or call binds to the new principal](img/rfc18-create-principal.svg)

### 2.2 Spellings

Each protocol names people its own way: NFSv3 by a number, SMB by a SID,
Kerberos by `name@REALM`, NFSv4 by `name@domain`. Each of these forms is a
**scheme**, and a principal's name in one scheme is one **spelling**. A
principal has at most one spelling per scheme. Some spellings are **stored**:
written on the `User` or `Group` record and as one row of the protocol-ID index
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)),
which answers "who holds this spelling?". Others are **derived** when needed,
from the name and a configured domain. Reading a spelling off the wire goes
through the index; writing one onto the wire reads the record.

| Scheme | Spelling | Unique among | Who has one | Stored |
| --- | --- | --- | --- | --- |
| `uid` | a 32-bit number | users | every user with a file spelling (§2.4), always (§2.3) | yes |
| `gid` | a 32-bit number | groups | every group, always (§2.3) | yes |
| `sid` | a Windows SID, binary form | users and groups together | every user with a file spelling, and every group (§4.4) | yes |
| `krb` | `name@REALM`, as the KDC spells it | users and machines | a user whose Kerberos name §4.2 does not derive; every machine | yes |
| `dir` | the directory's immutable entry ID | users and groups | every directory principal (§5.1) | yes |
| name | the record's `Name` | users; groups | every user and group but a machine | yes |
| `oidc` | (issuer, subject): an ID token's `iss` and `sub` | users | accounts linked to an issuer (§8.2); no file protocol carries it | yes |
| `owner` | `name@domain` | — | derived from the name and `identity.nfs4_domain` (§4.5) | no |
| `qualified` | `DOMAIN\name` | — | derived from the name and the NetBIOS name of the installation or the directory (§4.6) | no |

- Every stored spelling **MUST** be unique in its scheme. A write that would
  give a second principal a spelling another holds **MUST** fail, as RFC 16's
  index makes it.
- The kind decides the scheme a number is in: uid 1500 and gid 1500 are two
  spellings of two principals, or of none.
- Nothing inside the service compares spellings. Every comparison of identity —
  a cache key, an open's principal
  ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), a client record's creator
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), a quota charge — compares
  `PrincipalID`s.
- `Scheme.Stored()` says which schemes have index rows; `Find` reads only
  those, and `Spell` derives the others.

*Example.* alice is `P-7f3a…`. Stored: uid 1500, SID `S-1-5-21-…-1107`, name
`alice`. Derived: `alice@corp.example` and `CORP\alice`. An `AUTH_SYS` call
with uid 1500 looks up the `uid` index and finds `P-7f3a…`; a `GETATTR` reads
alice's record and writes `1500`.

**An e-mail address is not a spelling.** A user's record **MAY** hold one,
`User.Email`
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
It is optional, never defaulted, and names no principal to any check: no
mapping, squash, grant, ACL, cache key or index reads it. It is set through the
management API
([RFC 23 §3.5](rfc-23-control-plane.md#3.5%20Users%2C%20groups%2C%20grants%20and%20netgroups)),
or, for a directory principal, read from the directory entry (§5.1), which
makes it read-only to the API. It is stored as given, and the API refuses a
value that is not a syntactically valid address. It is personal data: it
**MUST NOT** appear in a log, a metric label, a span attribute or a logon event
([RFC 25 §5.4](rfc-25-observability.md#5.4%20What%20is%20never%20logged)), and
the API returns it as [RFC 23 §5.2](rfc-23-control-plane.md#5.2%20Roles) says.

> ponytail: no index and no lookup by e-mail address. Add a domain-folded index
> when a feature must find a principal from an address, such as an API sign-in
> by e-mail; uniqueness then needs deciding, since a directory may give two
> entries one address.

### 2.3 IDs are never defaulted and never reused

**The stable-ID rule.** An ID given to a principal is never given to another:

1. `PrincipalID` is drawn at random and never reissued
   ([RFC 16 §2.2](rfc-16-metadata-store.md#2.2%20Identity%20types)).
2. A uid, gid or RID (the last part of a SID, §4.4) that the installation
   allocates is drawn from a counter per scheme that only rises, inside the
   transaction that writes the record. An allocated ID **MUST NOT** be
   allocated again, even after its principal is deleted.
3. A uid or gid the operator assigns, or the directory supplies, **MAY** be any
   value no current principal holds. 0 is root's and 65534 anonymous's
   (§2.1): neither is ever allocated, and either is refused for any other
   principal — a directory entry whose `uidNumber` or `gidNumber` is one of them
   gets an allocated one instead, and the mint is counted. Assigning a deleted
   principal's uid to a new one gives the new principal no file of the old
   one's, because files name principals, never uids.
4. A missing uid or gid **MUST NOT** be filled with a default. A user created
   without one, or minted from a directory entry that has none, gets one
   allocated from `identity.id_range` (default 1 000 000 to 1 999 999 999). The
   write names the IDs the store allocates; a zero ID is never read as "allocate
   one" ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)).
   A principal with no file spelling holds none at all (§2.4).

> decision: a missing uid is allocated, never defaulted. A default makes every
> principal without one the same principal to every check that reads a uid —
> `AUTH_SYS` admission, an NFSv3 attribute, a cache key — and that is the bug
> the opening example walks through. The cost is that an operator who creates
> a user without a uid sees a number they did not choose; they may change it
> before the user owns anything. Overturned only by a client protocol that
> must derive a uid from another spelling and cannot be given an allocated one.

**Changing a spelling.** An operator **MAY** change a user's uid or a group's
gid. The change writes the record and its index rows in one transaction and
touches no file. Over NFS the principal's files then show the new number;
`AUTH_SYS` clients that still send the old one act as whoever now holds it, or
as anonymous (§4.3). A SID **MUST NOT** change once the principal has one: it is
what SMB clients cache and store in their own ACLs.

### 2.4 Principals with no file spelling

**A principal with no uid is not a file principal.** On files it acts as the
share's `Anonymous` principal (§6.2), with anonymous's groups and every flag
false, and no share grant may name it: the management API refuses one. Two
kinds of principal follow from this rule.

- **A machine principal** is a host or service signing in for itself: the
  `host/build01.corp.example@CORP.EXAMPLE` or `nfs/build01…` name an NFS client
  uses for its lease and state operations, or a machine account such as
  `BUILD01$@CORP.EXAMPLE`. A Kerberos name whose first component is a service
  name (`host`, `nfs`, `cifs`, or any name with a `/`), or whose user part ends
  in `$`, is a machine name. Its first successful Kerberos sign-in with no
  record **MUST** mint it, held as a `User` record of kind `Machine` with its
  `krb` spelling and nothing else
  ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
  It is still a principal of its own where state is compared: it is the caller's
  `Mapped` (§6.1), which NFSv4.1 state protection records
  ([RFC 21 §3.3](rfc-21-nfs.md#3.3%20Trunking%2C%20server%20owner%20and%20scope)),
  so two machines are two principals.
- **An API-only account** is a user record that exists only to sign in to the
  management API: a role, no uid, gid or SID, and no NT hash — the bootstrap
  administrator, a service account
  ([RFC 23 §5.1](rfc-23-control-plane.md#5.1%20API%20accounts%20are%20principals%20with%20a%20role)).
  No file protocol can present it, no ACL entry or owner can name it, and
  `Find` never returns it.

Giving an API-only account a file spelling — any scheme but `oidc` — makes it a
file user, and allocates the file spellings it still lacks in the same
transaction. A machine principal is never given one: a machine that needs file
access is linked to a user instead, by moving its `krb` spelling to that user
(§4.2), and its next sign-in binds to the user (§3.1).

*Example.* The bootstrap administrator `admin` and a CI account `deployer` can
create shares and grants, but cannot mount one.

> decision: machine principals are minted on sight, user principals are not
> (§3.3). A machine needs a principal of its own for state protection to mean
> anything, and minting one grants no file access, since it acts as anonymous
> and no grant can name it. Revisit if a KDC is shown to issue machine tickets
> to names the operator does not control; `identity_mints_total{kind="machine"}`
> is the signal.

### 2.5 Groups and membership

A principal's **groups** are every group it belongs to, directly or through
other groups: if alice is in `Accounts` and `Accounts` is in `Finance`, alice is
in both. This full set is the **closure**: the `Membership` records of the store
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)),
nested groups included, and the groups a directory or a verified PAC reports
(§5, §3.4).

- The closure **MUST** be computed whole, or not at all. A partial list is not
  merely incomplete; it can grant access. A file whose ACL says "deny `Finance`
  write, allow everyone write" denies alice only if `Finance` is in her list.
  Leave it out because one lookup timed out, and alice can write. A resolution
  that cannot read every membership — a store error, a directory timeout —
  **MUST** fail. A cycle is not an error: each group is visited once, and only a
  failed read or the cap fails the closure.
- A principal in more than 1 024 groups **MUST** be refused at resolution, with
  the count logged. A group's membership never makes a principal `admin`; only
  a grant does (§6.5).

> decision: a principal over the group cap is refused, not truncated. A
> truncated group list is a wrong answer that looks right, and it can drop the
> group a deny entry names. 1 024 sits at the SID limit of a directory-issued
> access token (Appendix A), so a real account rarely meets it.

> ponytail: the cap is a constant. Make it a setting, or per share, when a
> deployment shows accounts above it.

### 2.6 Components, wiring and lifecycle

One component, the **identity server**, holds the authenticator (§3),
`Identities` (§4), the directory client and its cache (§5), resolution and the
caller cache (§6, §7), and the management API's account checks (§8.1).
`node.New` ([RFC 23 §2.1](rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components))
builds it once on every node from one `Deps`. It builds no other component and
closes nothing it was handed. Its consumers get only the interfaces they call:

| Interface | Consumer | Handed as |
| --- | --- | --- |
| `Resolver` (§6.1) | `*vfs.Server`, for admission | `vfs.Deps.Identity` ([RFC 17 §2.3](rfc-17-vfs.md#2.3%20Dependencies%20and%20wiring)) |
| `Authenticator` (§3.1) | `*vfs.Server`, behind `vfs.Service`'s `SignIn` | `vfs.Deps.Auth` |
| `Identities` (§4.1) | `*vfs.Server`, behind `vfs.Service`'s `Spell` and `Find` | `vfs.Deps.Names` |
| `APISignIn`, `TokenVerifier` (§8.1) | the management API | its `Deps` ([RFC 23 §2.1](rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components)) |
| none | the adapters, which reach sign-in and naming only through `vfs.Service` ([RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity)) | — |

**A forwarded `Identity` is trusted because of the channel, not the value.** The
gateway forwards the adapter's `Identity` as a value to the file's primary,
which re-verifies nothing the gateway node's sign-in proved; it accepts the
`Identity` only because node channels are mutually authenticated
([RFC 15 §4.4](rfc-15-topology.md#4.4%20Node%20channels)), so only a node of the
installation can send one. A node's channel credential therefore asserts any
principal, and is as sensitive as the keytab. The primary **MUST** refuse, as
`ErrInvalid`, a forwarded `Identity` that is inconsistent with itself — evidence
with a principal at a proof kind below `Secret`, or none at `Secret` or above —
so a gateway bug becomes a refusal, not an impersonation. It resolves with its
own caches and re-reads the bound principal's record (§6.1), so a disable
reaches calls forwarded from every node.

> decision: identity is one component shared by `*vfs.Server` and the
> management API, not part of `vfs`. The API signs in over no file protocol, so
> a sign-in path inside `vfs` would have the API depend on the filesystem
> service; a handshake round is per connection and acts on no share; and
> identity has its own suite (§10), run without a filesystem. Overturned if the
> API stops calling identity, leaving `*vfs.Server` the only consumer; identity
> then folds into `internal/vfs`.

```go
package identity // internal/identity

type Deps struct {
	Principals metadata.Principals // RFC 16 §3: lookups by spelling, groups, grants, a share's policy
	Records    metadata.Records    // RFC 16 §3: one principal's record
	Mint       metadata.Minter     // RFC 16 §3: the only writes (§2.4, §5.1)
	Secrets    SecretProvider      // RFC 13 §7: keytab, NT hashes, API password hashes, the bind credential, the machine-account password
	Logons     LogonSink           // RFC 25 §4.1's logon stream
	Issuers    IssuerKeys          // fetches each OpenID Connect issuer's keys over verified TLS (§8.2)
	Clock      Clock // the package's own subset of RFC 8 §2.1's Clock
	Metrics    *metrics.Registry
	Logger     *slog.Logger
}

// New takes no settings and starts nothing. *Server implements Authenticator,
// Identities, Resolver, APISignIn and TokenVerifier; only the composition root
// holds it.
func New(d Deps) (*Server, error)
func (s *Server) Apply(st State) error         // from the node, first before Start; refuses an invalid setting and keeps the last applied
func (s *Server) Start(ctx context.Context) error // after the first Apply; the directory client dials lazily, so a down directory does not fail start
func (s *Server) Stop(ctx context.Context) error  // drops every in-memory table

// State is everything Apply delivers, swapped in as one snapshot
// (atomic.Pointer[state]): a call reads one State for its whole life, never a
// generation from one Apply and a setting from another.
type State struct {
	Generation uint64          // the principals generation (§7.2)
	Settings   config.Identity // the identity.* settings and the directory record
	Keytab     []SecretVersion // the keytab's versions in force (§3.3)
	NTLM       Revision        // the identity.ntlm setting's revision (§3.5)
}
```

`Records` and `Minter` are RFC 16's views, defined there once. A `Minter` write
never overwrites a record, allocates the IDs it is told to inside its
transaction, and fails with `ErrExist` when another principal holds a row it
would write (§2.2).

**Start and stop.** No settings reach `New`. The node reads the records at
start and hands identity its first `State` through `Apply`, before `Start`. It
stops identity after `*vfs.Server` and the management API, since a drain still
delivers calls that resolve and the API checks an account on every call; the
order is [RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop)'s.

**Reconfiguration** arrives only through `Apply`, from the node's reconcile
([RFC 23 §2.3](rfc-23-control-plane.md#2.3%20The%20node%20reconciles%3B%20it%20does%20not%20take%20orders)).
The node that wrote a change applies it at once rather than at its next read,
so an operator's own next call sees it.

| Change | Effect |
| --- | --- |
| a principal, membership or protocol-ID row | a new generation; cached callers built under an older one are not served (§7.2) |
| a keytab `Secret` version | the new key is added, and an older one is kept for one maximum ticket lifetime (§3.3); no restart |
| the directory record | a new client and an empty directory cache; lookups in flight finish on the old client |
| `identity.ntlm` turned off | as §3.5 says |
| `identity.id_range` | applies to the next allocation; no existing ID moves |

**Restart.** Every table is in memory and is lost: evidence (clients sign in
again), the replay cache (§3.3), the logon throttle (§3.1) and the caches, which
start cold.

### 2.7 Package layout

The contract is `pkg/identity`, a leaf importing metadata types only: the value
types (`Identity`, `ProofKind`, `Protection`, `Claims`, `Evidence`, `Keys`,
`SignInStep`, `SignInResult`, `Caller`, `Spelling`, `Scheme`, `SID`,
`NTLMAuthenticate`), the errors, the interfaces of §3.1, §4.1, §6.1 and §8.1,
and `NewHashes`. Its test helpers are `identitytest`: `Static`, each interface
returning fixed values, and a fake directory. The server is `internal/identity`,
with the LDAP client alone in `internal/identity/directory` so only it imports
LDAP; only `internal/node` imports the server. `pkg/vfs` aliases the value types
an adapter needs ([RFC 17 §3.0](rfc-17-vfs.md#3.0%20Value%20types)), so an
adapter imports `pkg/vfs` alone
([RFC 20 §2.3](rfc-20-adapters.md#2.3%20What%20an%20adapter%20may%20import)) and
neither identity package. The NTLM and SPNEGO wire code is
[RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity)'s.

## 3. Authentication

### 3.1 The authentication interface

Adapters run handshakes; this interface does the parts that need a secret or a
directory. Its one caller is `*vfs.Server`, which passes `vfs.Service`'s
`SignIn` straight through, on the node holding the connection
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)). Signatures are indicative:

```go
package identity // pkg/identity

// Authenticator verifies one handshake round. It is the same shape as
// vfs.Principals.SignIn, so the server passes it through.
type Authenticator interface {
	SignIn(ctx context.Context, step SignInStep) (SignInResult, error)
}

// SignInStep is one verification round: an AP-REQ for Service, or a decoded
// NTLM AUTHENTICATE with the challenge the adapter sent. NTLM's NEGOTIATE and
// CHALLENGE never reach identity: the adapter answers them and passes the
// challenge here.
type SignInStep struct {
	Kerberos  []byte            // an AP-REQ
	Service   Service           // NFS, CIFS, Host: the keytab service name a ticket is for
	NTLM      *NTLMAuthenticate // or an NTLM response to Challenge
	Challenge [8]byte
	Client    netip.Addr
	Context   *Evidence         // the context's current evidence, on a re-authentication (§3.8); nil otherwise
}

// SignInResult is an Identity with proof kind Secret (NTLM) or Ticket
// (Kerberos) and protection None, which the adapter raises per request as its
// protocol's signing or sealing allows, and the Keys beside it, never in it.
type SignInResult struct {
	Identity Identity
	Keys     Keys
}

// Evidence is what one sign-in established, and nothing more. It holds no key,
// so it is safe to copy into every request's Identity. Its digest, the
// evidence part of the proof key (§7.1), is computed once, here.
type Evidence struct {
	Name      string             // canonical: "alice@CORP.EXAMPLE", "CORP\\alice", a local user name
	Principal metadata.Principal // what sign-in bound Name to; every call acts as it
	SIDs      []SID              // from a verified PAC or a pass-through reply: user SID first; else empty
	DenyOnly  []SID              // marked deny-only by the signer (§3.4)
	Expires   time.Time          // the ticket's end time; zero for NTLM
	digest    [32]byte
}

// Keys is what the adapter's mechanism needs for the AP-REP and for signing
// and encryption. It is never copied into an Identity.
type Keys struct{ Session, Subkey, APRep []byte }

type Service uint8
// NTLMAuthenticate is the decoded AUTHENTICATE message the adapter's NTLM
// mechanism hands over, with the three messages' bytes kept exactly as sent so
// the MIC can be checked (§3.5).
type NTLMAuthenticate struct {
	User, Domain, Workstation       string
	NTResponse, EncryptedSessionKey []byte
	MIC                             []byte
	Flags                           uint32 // NegotiateFlags, and the AV pair MsvAvFlags
	AVFlags                         uint32
	Negotiate, Challenge, Raw       []byte // the wire bytes of NEGOTIATE, CHALLENGE and this AUTHENTICATE
}
```

**Evidence is bound to one principal for its life.** Before it returns, `SignIn`
**MUST** map what it verified to a principal and set `Evidence.Principal`: a
local NTLM user by the record it verified against, a Kerberos name by §4.2, an
NTLM pass-through answer by its verified user SID (§4.4), never by its
`DOMAIN\user` name. Evidence that maps to no principal fails as §3.3 and §3.5
say. Every call on that security context then acts as that `PrincipalID`:
resolution checks the principal's record on every call (§6.1) and refuses it if
disabled or deleted (§3.7), and **MUST NOT** map the name again. A name later
linked to, or reused by, another principal reaches the new one only through a
fresh sign-in.

- Every error `SignIn` returns **MUST** be one of `ErrLogonFailure`, for a wrong
  credential or an account that cannot sign in (the logon event's outcome says
  which, §9.2); `ErrInvalid`, for a malformed message; or `ErrUnavailable`, for
  an answer that could not be decided (a store, keytab or directory fault). The
  adapter answers each as the protocol's logon failure
  ([RFC 20 §4.4](rfc-20-adapters.md#4.4%20Identity)), and **MUST NOT** turn any
  of them into a guest or anonymous session (§3.6).
- `SignIn` reads only the store views, the directory cache (§5.2) and the
  secret provider, writes only the records a first sign-in mints (§2.4, §5.1),
  and **(cluster)** runs on the node that holds the connection.
- Evidence is valid until the security context ends, or until `Expires`. A
  request on a context past `Expires` **MUST** be refused with the protocol's
  expired-credential answer, but for the calls §3.8 lets through, so the client
  renews.
- **Repeated logon failures are throttled here**, not in an adapter: the
  authenticator sees every attempt from both protocols with the client's
  address. Past 10 `ErrLogonFailure` results in 10 minutes for one (principal
  name, client address), every further attempt from that pair **MUST** fail
  `ErrLogonFailure` before its credential is checked, until the window has
  passed with fewer. The answer is the ordinary logon failure, so it tells a
  guesser nothing, and no account is locked: a local account is never locked by
  failures, and a domain account's lockout is the domain's.
- **Each attempt emits one logon event** to
  [RFC 25](rfc-25-observability.md#4.1%20Four%20streams)'s `logon` stream:
  success or failure with its outcome, the mechanism, the name as presented,
  the principal on success, and the client address. It **MUST NOT** carry the
  credential: no response, ticket, hash or session key.

> ponytail: throttling is in memory, per node. A restart forgets it, each node
> counts alone, and a guesser who spreads one name across many addresses, or
> many names across one, stays under it. Keep the counts in the metadata store,
> or add a per-address limit across names, when the logon stream shows a spray
> that this rule did not slow.

### 3.2 AUTH_SYS and AUTH_NONE prove nothing

`AUTH_SYS` carries a uid, a primary gid and up to 16 supplementary gids, as the
client asserts them. It is decoded by the NFS adapter and never signs in.

- The uid and gids are taken as sent, and mapped through the `uid` and `gid`
  indexes on every call (§4.3); nothing about them is verified. The
  credential's machine name and stamp **MUST** be ignored.
- An `AUTH_SYS` request reaches the service with proof kind `Claimed` and its
  ids as `Claims` (§6.1); `AUTH_NONE` with `None` and no claims. A credential
  that does not decode, exceeds the wire's limits, or uses any other flavour is
  refused before it is a request ([RFC 21 §11.1](rfc-21-nfs.md#11.1%20Identity)).
- A `Claimed` caller is never `authenticated`, and an `admin` grant confers
  nothing on it (§6.5). The only thing that makes an asserted uid worth anything
  is the network it came from, which the share's `MinProof` and client rules
  decide ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)).
- A `None` caller acts as the share's anonymous principal (§6.2).

**Supplementary groups past 16.** A share whose policy sets `ManageGIDs`
replaces the credential's gids with the closure of the mapped principal's
memberships (§2.5). Without it, the gids sent are the caller's groups.

> decision: `ManageGIDs` is off by default. With it on, a principal's groups are
> what the store says, which is right where the store is the source of truth
> for groups and wrong where clients get groups from a local `/etc/group` the
> store has never seen. Turn the default on if most deployments are shown to
> hold groups in the store or a directory.

### 3.3 Kerberos

Kerberos is accepted over RPCSEC_GSS ([RFC 21 §10](rfc-21-nfs.md#10.%20RPCSEC_GSS))
and inside SMB's SPNEGO ([RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in)).

- **Service names.** The installation accepts tickets for the service names its
  keytab holds — `nfs/<host>`, `cifs/<host>` and `host/<host>` — for every host
  name a node answers on. A ticket for any other name **MUST** be refused.
- **The keytab is a secret**: each key a `Secret` of kind keytab
  ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)), loaded only through
  `Apply`. A key rotates in this order: the new version is added to the keytab
  `Secret`; once every node reports it applied
  ([RFC 23 §2.4](rfc-23-control-plane.md#2.4%20What%20a%20node%20reports)), the
  key is changed at the KDC; the old version is kept for one maximum ticket
  lifetime after that, 10 hours plus the clock skew, then removed. So tickets
  under either key are accepted during the rollover, and a retired key is not
  accepted forever.
- **Checks.** `SignIn` **MUST** verify the ticket's encryption, its start and end
  times within a clock skew of 5 minutes, as Kerberos implementations use, the
  authenticator's freshness against a replay cache held in the node's memory,
  and, over SMB, the client-address rule RFC 22 §4.1 states.
- **The name.** The evidence's `Name` is the ticket's client name and realm,
  exactly as the ticket spells them. Mapping it is §4.2's, done once, at
  sign-in.
- **An authenticated user name that maps to no principal** — no `krb` row, no
  local user by §4.2's rule, and no directory entry — **MUST** be refused with
  `ErrLogonFailure`. A machine name is minted instead (§2.4).

> ponytail: the replay cache is in memory, per process. A restart forgets it, so
> an AP-REQ captured within the clock skew before a restart can be accepted once
> after it, and **(cluster)** each node keeps its own. A replay yields no session
> key, so it can sign no SMB request and compute no RPCSEC_GSS verifier. Refuse
> authenticators stamped before the process start, or share the cache between
> nodes, when a replay across a restart or a fail-over is shown to reach
> anything.

> ponytail: the clock skew and the old key's retention are constants. Make
> either a setting when a deployment's KDC is shown to need another value.

> decision: an unknown user principal is refused, not made anonymous. A
> Kerberos user the installation has no record of is either a directory
> misconfiguration or someone outside the organisation the KDC trusts; acting as
> anonymous would hide the first and admit the second to every share with an
> everyone grant. Revisit if a deployment is shown to need cross-realm users
> with no directory, and then as a share-scoped option.

### 3.4 The PAC

A Kerberos ticket from a directory that issues PACs (MS-PAC) carries one: the
user's SID, its group SIDs and their attributes, signed by the domain
controller.

- The PAC's server signature **MUST** verify under the service key before any
  SID in it is used. A PAC that does not verify **MUST** fail the sign-in. A
  ticket without a PAC is accepted, its groups then read from the store and the
  directory (§2.5); so is a PAC whose server signature verifies but which holds
  no logon-information buffer, which is treated as no PAC.
- **The PAC is bound to the record.** Its user SID **MUST** equal the SID on the
  record §4.2 selected, and its client name and authentication time **MUST**
  equal the ticket's. A mismatch fails `ErrLogonFailure`, logs both principals,
  and binds nothing. A name reused in the directory after its first holder was
  deleted, or a local user and a directory user sharing one, is then refused
  rather than bound to the wrong person.
- The evidence's `SIDs` are the user SID and the group SIDs the PAC lists — its
  memberships, extra SIDs and resource groups — excluding any whose attributes
  mark them disabled. A SID in the installation's own domain (§4.4), in the
  built-in domain (`S-1-5-32-…`), or naming a user rather than a group, is
  dropped and counted: the directory's domain controller can vouch only for its
  own domain's groups. Well-known SIDs are kept only where RFC 22 §16.3 lists
  them.
- **A deny-only SID is kept, apart.** A SID the PAC marks deny-only (Windows'
  `SE_GROUP_USE_FOR_DENY_ONLY`) is one the directory says may only take access
  away — as in a restricted administrator token. It **MUST** be carried in the
  evidence's `DenyOnly`, and from there in the caller's (§6.3). It matches only
  deny entries ([RFC 19 §2.3](rfc-19-authorization.md#2.3%20Principals)), never
  a share grant, and adds none of the groups it is a member of. Dropping it would
  lose its denies; folding it into `SIDs` would let it grant.
- PAC groups are the caller's groups as of the ticket's issue, so a group
  removed in the directory leaves a PAC-bearing session at the ticket's
  `Expires`, the bound for that case. Resolution unions them with the store's
  memberships; a PAC never removes a group the store records.
- **The PAC supplies groups only.** A directory principal's enabled state is
  read through the directory cache on every proof, PAC or not (§5.3).

> decision: only the PAC's server signature is verified; the KDC signature is
> not sent to a domain controller. A forged PAC needs the service key, which the
> keytab's seal protects. Overturned if the keytab is ever readable without its
> wrapping key, or a deployment requires KDC-signature checks.

### 3.5 NTLMv2

NTLMv2 is accepted inside SPNEGO when `identity.ntlm` is on (the default).
NTLMv1 and LM are refused ([RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in)).
`SignIn` decides one message from one of two sources:

| The message's domain, compared case-insensitively | Source | How |
| --- | --- | --- |
| empty, or the installation's own NetBIOS name, which the CHALLENGE advertises when no pass-through is configured | **local** | the user is found by name; its NT hash, a `Secret` of kind nt-hash, verifies the NTLMv2 response computed with the user and domain as the message names them (MS-NLMP 3.3.2). A directory principal is refused: it has no local NT hash |
| the directory's NetBIOS domain, with pass-through configured | **pass-through** | the response is sent once to a domain controller over the secure channel of the installation's machine account (MS-NRPC `NetrLogonSamLogonEx`), sealed and signed with AES; the reply carries the user's SID and group SIDs with their attributes, used as a PAC's are (§3.4); the evidence maps by that user SID |
| anything else | — | `ErrLogonFailure` |

- **The MIC.** When the AUTHENTICATE's `MsvAvFlags` marks a MIC present, the MIC
  **MUST** verify over the exact NEGOTIATE, CHALLENGE and AUTHENTICATE bytes on
  the wire (MS-NLMP 3.2.5.1.2), and a flagged MIC that is missing or zero fails.
  Either failure is `ErrLogonFailure`. A message that asks for signing without
  key exchange is refused the same way.
- **Equal work.** The NTLMv2 proof is compared in constant time, and an unknown
  name costs the same work as a wrong password.
- **NT hashes exist only while NTLM is on**, and only for a user that holds file
  spellings and is not a directory principal: an API-only account keeps none,
  whatever its password. An NT hash is equivalent to the password for NTLM.
  `NewHashes` (§8.1) is the one place this rule is applied.
- **NTLM is weaker than Kerberos**, and the proof kind says so: an NTLM session
  is `Secret`, a Kerberos one `Ticket`. A share whose `MinProof` is `Ticket`
  refuses NTLM.
- **`ProofSecret` comes only from NTLMv2**, local or pass-through. While
  `identity.ntlm` is off, resolution refuses every `Secret` identity, and the
  setting arrives in a new `State`, which no cached caller built under the old
  one survives (§7.1), so no cached caller serves an established NTLM session.

**Turning NTLM off** refuses NTLM sign-ins and every call on an established NTLM
session from the `Apply` that delivers the setting — within a second of its
commit (§7.2) — and deletes every stored NT hash, as an operation in bounded
batches ([RFC 23 §4.6](rfc-23-control-plane.md#4.6%20Long-running%20operations)).
A password set derives its NT hash under the `identity.ntlm` revision it read,
and the API's write of it is conditional on that revision still being current,
so no NT hash is written after the deletion has passed its user. Turning NTLM on
again stores a hash only at a user's next password set.

> decision: turning NTLM off refuses established NTLM sessions as well, rather
> than letting them run to their end, because an operator turning it off is
> removing a weaker credential. Overturned if operators are shown to need a
> drain window.

> decision: NTLM is on by default. Workgroup deployments with local users have
> no KDC, and SMB clients outside a domain fall back to NTLM. A deployment with
> Kerberos everywhere turns it off. Revisit the default once local users are
> shown to be rare.

**The machine account.** Pass-through needs the installation joined to the
directory's domain as a computer account. Joining is an operation
([RFC 23 §4.6](rfc-23-control-plane.md#4.6%20Long-running%20operations)) that
creates the computer object, sets its service names, and stores its password as
a `Secret`. Domain controllers are found from the domain's DNS service records,
with fail-over between them. The password rotates on a schedule shorter than the
domain's maximum machine-password age, always domain controller first, then a
new `Secret` version
([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)); a rotation **MUST NOT**
start until every node reports the previous one applied, because a domain
controller accepts only the current and the previous password, and a second
rotation would leave a lagging node with neither. A failure to store the new
version after the domain controller took it leaves a password only that
controller knows, and needs a re-join.

> ponytail: pass-through reaches only the one directory's domain; §5.1's
> ponytail says when to follow trusts.

### 3.6 Anonymous and guest sessions

An SMB client may set up a session that proves nothing: an anonymous one, with
no user, or a guest one, naming the guest account.

- Both **MUST** be set up as **anonymous** sessions: the identity is `None`, the
  caller is the share's `Anonymous` principal (§6.2), and the session holds no
  secret key.
- Which shares each may reach is
  [RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in)'s: a null session `IPC$`
  only, a guest also a share whose policy enables guest access and does not
  encrypt. This RFC widens nothing.
- A session setup whose credential fails **MUST** fail. It **MUST NOT** become
  an anonymous or guest session — not for an unknown user, not for a wrong
  password, not for a directory error.

> decision: no "map to guest", and guest is proof `None`. A server that answers
> a wrong password with a guest session turns every typo into a session the
> client did not ask for, and a directory outage into access. A guest proves
> nothing a null session does not, so one anonymous principal covers both, and
> the only difference between them, which shares admit a guest, is a share
> setting RFC 22 applies. Revisit if a deployment needs a guest told apart from
> anonymous in a file's owner or an ACL entry.

### 3.7 Disabled and deleted principals

- A principal whose record is `Disabled`, or marked deleted
  ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)),
  **MUST** fail sign-in with `ErrLogonFailure` (logon outcome `disabled`), and
  **MUST** be refused at resolution on every call, whatever evidence its
  connection holds. The second rule is what stops a session or GSS context set
  up before the change, within §7.2's bound.
- A directory account is disabled when the directory says so, or when it
  answers "no such entry" for a principal it once minted (§5.1).
- A disabled user's open handles stay open; every call through them is refused.

### 3.8 Re-authentication

Some protocols re-run sign-in on a security context that already exists — SMB
re-authenticates a session in place, keeping its trees and opens, to renew a
ticket ([RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in)). The adapter passes the
context's current evidence in `SignInStep.Context`.

- A re-authentication happens in place: a successful one replaces the
  context's identity with the one the new proof establishes, in one step, and
  keeps everything else the context holds, its open handles with the access
  each was granted at its open. Why, and its ceiling, is
  [RFC 22 §4.1](rfc-22-smb.md#4.1%20Signing%20in)'s decision.
  A re-authentication as anonymous or guest succeeds like any other; the
  context keeps the keys its first sign-in derived.
- Any failed re-authentication ends the context it targeted. The old evidence
  never outlives a failed proof, such as one after a password change.
- A context past `Expires` still admits its re-authentication and the calls that
  only release state — close, unlock, a break acknowledgement, logoff — so a
  client can release what it holds while it renews
  ([RFC 19 §3.1](rfc-19-authorization.md#3.1%20One%20request%2C%20in%20order)).
  Every other request on it is refused with the expired-credential answer.

## 4. Identity mapping

### 4.1 Two directions, one mapping

Mapping runs in two directions, and both use the same records:

- **Wire to principal**, on every call and every name a client sends: a
  Kerberos name, a uid, a SID, an `owner@domain` string, a name in an `lsarpc`
  lookup. It reads the protocol-ID index.
- **Principal to wire**, whenever an adapter shows a stored principal: a file's
  owner and group, an ACL entry's principal, a quota entry. It reads the
  principal's record.

`*vfs.Server` holds both as `Identities`, and `vfs.Service` exposes them to
adapters as `Spell` and `Find`
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), so an adapter asks and never
decides:

```go
package identity // pkg/identity

// Identities maps principals to spellings and back (§4). It acts on no share
// and is not admitted; vfs.Deps.Names holds it (§2.6).
type Identities interface {
	// Spell returns p's spelling in scheme, or ErrUnknownPrincipal.
	Spell(ctx context.Context, p metadata.Principal, scheme Scheme) (Spelling, error)
	// Find returns the principal holding s, or ErrUnknownPrincipal.
	Find(ctx context.Context, s Spelling, o FindOpts) (metadata.Principal, error)
}

// FindOpts: Mint lets a lookup the directory answers mint the directory
// principal. The server sets it only for a call whose Identity is Secret or
// above (§4.4).
type FindOpts struct{ Mint bool }

type Scheme uint8 // UID, GID, SID, Krb, Dir, UserName, GroupName, OIDC, Owner, Qualified (§2.2)
func (s Scheme) Stored() bool // true for the schemes with index rows
// Spelling is one protocol's name for a principal: Num, SID or Text is set, by Scheme.
type Spelling struct{ Scheme Scheme; Num uint32; SID SID; Text string }
```

Sign-in, `Find` and resolution (§4.3) share one internal mint, so every record
identity writes goes through one path, under one rule: it never overwrites, it
allocates what the record lacks in its own transaction, and on `ErrExist` it
re-runs the lookup that led to it once. If the lookup now finds a principal, the
mint binds to it; otherwise it fails `ErrUnavailable`, writes nothing and logs
both `PrincipalID`s. Two nodes minting alice find alice by name or SID on the
re-run; bob claiming alice's `uidNumber` finds no bob, and fails.

- An adapter **MUST** encode and decode every principal through these calls,
  and **MUST NOT** derive one spelling from another — a SID from a uid, a uid
  from a RID, a name from a SID.
- A lookup error other than "not found" **MUST** reach the client as a server
  failure, never as "unknown principal": a store outage that made every owner
  `nobody` would be read by a client as data.
- An adapter's ACL translation takes its principal names from these two calls
  ([RFC 19 §5.1](rfc-19-authorization.md#5.1%20One%20translation%2C%20two%20spellings)),
  passing `ErrUnknownPrincipal` and every other error through unchanged.

### 4.2 Kerberos names

A Kerberos name maps to a principal in this order, stopping at the first that
answers:

1. the `krb` index row for the name exactly as the ticket spells it — an
   operator's explicit link, to a user or to root, or a minted machine;
2. if the realm is one of the keytab's realms, the local user whose name equals
   the name's user part, compared as user names are (§4.6), unless it is a
   directory principal or has no file spelling;
3. the directory, if the realm is the directory's: the entry the directory
   holds under the name's user part, by the record's name attribute (§5.1), and
   then the principal holding that entry's `dir` spelling — never a principal
   found by name alone;
4. otherwise none: a user name is refused (§3.3), a machine name minted (§2.4).

A realm in the name **MUST** be compared as the KDC spells it, case and all. A
name with a service part (`nfs/build01`) never matches step 2 or 3. A ticket
with a PAC is then checked against the record §3.4 says.

### 4.3 uid and gid

- A uid maps to the user holding it in the `uid` index, a gid to the group in
  the `gid` index; uid and gid 0 map to root and 65534 to anonymous (§2.1). With
  a directory configured, a claimed uid or gid with no row is asked of the
  directory by `uidNumber` or `gidNumber`, and an entry that answers is minted
  as a directory principal under §5.5's guards. Such lookups are capped per
  client address, and such mints per node (§7.3).
- **An `AUTH_SYS` uid with no principal behind it**, after that lookup, maps
  to anonymous at mapping time, before any squash applies (§6.1): `Mapped` is
  the anonymous principal, the caller then acts as the share's `Anonymous` as
  a `None` caller does (§6.2), and the call is counted
  (`identity_unmapped_total{scheme="uid"}`).
- **A call that names no share** — a client registration such as NFSv4's
  `EXCHANGE_ID` or `SETCLIENTID` — is mapped under the installation's
  defaults, never a share's mapping, since no share's policy applies to it
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)).
- **A credential gid with no principal** is no group at all: it is dropped from
  the caller's groups, and a create falls back to the principal's primary group
  ([RFC 7 §2.4](rfc-7-namespace-metadata.md#2.4%20Attributes%2C%20and%20who%20writes%20them)).
- **A principal shown over NFS** is its uid or gid. A principal with no spelling
  in that scheme — a well-known principal other than root and anonymous as a
  file's owner, a deleted principal — is shown as the anonymous uid or gid,
  65534.
- **A directory principal's uid and gid** are the entry's `uidNumber` and
  `gidNumber` when it has them, else allocated (§2.3); written once, with the
  principal, and never recomputed, so a later change of the directory's
  attribute changes no existing principal.

> decision: an unmapped `AUTH_SYS` uid acts as anonymous rather than as a
> principal minted for that number. Any client may send any of 2^32 uids, so
> minting on sight would let one host fill the store with principals, and each
> would own files no operator can name. The cost: a client user with no record
> creates files owned by anonymous. Add a share option that mints numeric
> principals, with a cap, when a deployment shows NFS users the store cannot
> hold records for.

> ponytail: one uid rule — the entry's numbers, else allocated. Add a mode that
> derives the uid from the SID's RID when a deployment must agree on uids with
> an existing server that maps that way, or a directory flag ignoring the
> entry's numbers when a directory is shown to hold wrong ones.

### 4.4 SIDs

- **Every user with a file spelling, and every group, has a SID**, written with
  its record. A local principal's SID is the installation's domain SID followed
  by a RID drawn from the RID counter (§2.3). A directory principal's SID is the
  directory's when its entry has one; an entry with none — an RFC 2307
  directory with no Windows attributes — is given a local SID when it is minted,
  and keeps it whatever the directory later says (ID4).
- **The installation's domain SID** is `S-1-5-21-a-b-c`, where `a`, `b` and `c`
  are the first twelve bytes of the installation ID
  ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities))
  read as three little-endian 32-bit numbers. It is derived, never stored, and
  never changes. A principal carried into another installation by an export
  keeps the SID it has
  ([RFC 26 §3.2](rfc-26-catalog-backups.md#3.2%20Import%20is%20staged%20and%20published%20atomically)).
- **A RID is never derived from a uid.** A uid can change (§2.3); a SID cannot.
- **Well-known SIDs** map to the well-known principals of §2.1, as
  [RFC 22 §16.3](rfc-22-smb.md#16.3%20Namespace) spells them.
- **A SID with no principal behind it** — a user of a domain the installation
  does not know — is `ErrUnknownPrincipal` from `Find`; what a set naming one
  does is [RFC 19 §2.8](rfc-19-authorization.md#2.8%20What%20a%20stored%20ACL%20may%20hold)'s.
  A SID of the directory's domain is asked of the directory first, and minted
  only when `FindOpts.Mint` is set; otherwise `Find` answers
  `ErrUnknownPrincipal` and writes nothing.
- **A principal shown over SMB with no SID** — only a deleted one — is shown as
  the null SID `S-1-0-0`.

> decision: a lookup by an unauthenticated caller never mints. `lsarpc` runs on
> `IPC$`, which an anonymous session reaches, so minting there would let a
> caller that proved nothing write one record per directory account. Overturned
> if an anonymous client is shown to need a stable principal for a SID it only
> reads.

> decision: a deleted principal is shown as the null SID, not as the SID it had,
> because deleting a user drops its index rows
> ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)) and
> its SID with them. The cost is that two deleted owners look alike in the
> security tab. Keep deleted principals' SIDs as tombstone rows when an operator
> needs to tell them apart.

### 4.5 NFSv4 owner strings

NFSv4 names owners and groups as strings; this section is the `owner` scheme,
and RFC 21 carries it on the wire.

- **Spelling.** On a call with proof kind `Secret` or above, a principal with a
  name is `name@domain`, where `domain` is `identity.nfs4_domain` (default: the
  keytab's first realm in lower case, or `localdomain` with none). With
  `Claimed` or `None` it is the decimal uid or gid, as RFC 8881 §5.9 allows. A
  principal with no spelling is `nobody@domain`, or `65534`.
- **Finding.** A string `name@domain` whose domain equals `identity.nfs4_domain`,
  compared case-insensitively, maps by name (§4.6). A decimal string maps by uid
  or gid, whatever the proof. Any other string, including a name in another
  domain, is `ErrUnknownPrincipal`, which RFC 21 answers `NFS4ERR_BADOWNER`.

> decision: numeric owner strings on `AUTH_SYS`. A client using `AUTH_SYS`
> already names users by number, and its idmapper may know none of the server's
> names; numbers keep `ls -l` showing what the client sent. Names on Kerberos,
> where the client and server share a realm and its names. Revisit if a client
> is shown to need names on `AUTH_SYS`.

### 4.6 Names

- User and group names are compared case-insensitively, under simple Unicode
  case folding, and **MUST** be unique under that comparison within their kind.
- A user and a group may share a name
  ([RFC 16 §3](rfc-16-metadata-store.md#3.%20Interfaces%2C%20by%20consumer)); a
  name lookup always says which kind it wants.
- The `qualified` scheme spells a local principal as `<NetBIOS name>\<name>` and
  a directory one as `<directory domain>\<name>`, as `lsarpc` lookups answer
  ([RFC 22 §5.2](rfc-22-smb.md#5.2%20IPC%24%20and%20the%20RPC%20services)).

## 5. Directory integration

### 5.1 What a directory provides

A directory is one external source of users and groups — an LDAP directory with
Windows attributes (MS-ADTS), or one with RFC 2307 attributes — named by the
installation's directory record (`identity.directory`): its address, base, bind
credential reference, trust roots, realm, NetBIOS domain, and the attributes
that hold a user's and a group's name (default `sAMAccountName`; `uid` and `cn`
for an RFC 2307 directory). There is at most one.

A directory answers three questions, each by a bounded number of queries under a
5-second timeout:

| Question | Asked when |
| --- | --- |
| who holds this name, SID, entry ID, `uidNumber` or `gidNumber`? | §4.2 step 3, §4.3 and §4.4 find no record |
| what are this principal's groups, nested? | resolving a directory principal's groups (§2.5) |
| what is this group's SID and gid? | minting a group a member's lookup names |

Each user answer carries the account's enabled state and, when the entry has
one, its e-mail address (its `mail` attribute, for example). A nested-group
closure the directory cannot answer in one query is walked level by level, and
one that cannot be finished fails whole (§2.5).

- **A record is minted once**, from the first answer, and holds its
  `PrincipalID`, `dir` entry ID, name, SID, uid and gid from then on. Later
  answers update its name through the store's rename and its e-mail address,
  each when it changed; a name another principal holds is refused and the
  record keeps its old one. Later answers never change its `PrincipalID`, entry
  ID, SID, uid or gid. An entry with no SID or no uid gets one allocated (§4.3,
  §4.4), so a directory without Windows or Unix attributes still yields
  principals that satisfy ID3.
- **An entry that is gone is a disabled account.** "No such entry" for a
  principal the directory once minted is read as disabled, never as "no
  change": that is how offboarding usually ends.
- **A directory principal's groups and enabled state** are not written to the
  store. They are cached (§5.2), so a change in the directory takes effect at
  the next lookup.
- **Queries are exact.** Every value placed in a query — a Kerberos user part,
  an NTLM or `lsarpc` name, an owner string's name, a SID — **MUST** be escaped
  as RFC 4515 requires, binary values as `\xx` escapes. A "who holds this"
  query answered by more than one entry fails `ErrUnavailable`, logged, and is
  never bound: the name attribute may not be unique in every directory.
- **The connection is verified.** It **MUST** use TLS, with the server's
  certificate chain verified against the record's trust roots, or the
  system's, and its host name matched; a record that asks for plain LDAP or for
  skipping verification is refused at validation. A referral to any other host
  is not followed, so the bind credential goes nowhere else. The bind
  credential is a `Secret` ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)).

> ponytail: one directory, read directly over LDAP: no forest trusts, no global
> catalogue, no second directory; NTLM pass-through and SID lookups reach only
> its domain (§3.5). Follow trusts, and route pass-through to the trusted
> domain's controller, when a deployment has users in a second domain or forest.

### 5.2 The directory cache

![The life of a directory answer: fresh for five minutes, usable while the directory is failing until twenty minutes after it was given, then refused; a cached caller built from it expires at five minutes. A local Disabled flag takes effect within a second through the principals generation, whatever the directory cache holds](img/rfc18-directory.svg)

The identity server keeps one cache of directory answers, in memory;
**(cluster)** one per node.

- A positive answer is fresh for 5 minutes. A "no such entry" answer is fresh
  for 30 seconds, so a user created in the directory a moment ago can sign in
  soon after.
- An error or timeout **MUST NOT** be cached as "no such entry", and **MUST
  NOT** replace a positive entry.
- Concurrent lookups of one key **MUST** share one query, so a sign-in storm
  sends the directory one query per user, not one per connection.
- The key is the question: (scheme, spelling) for "who holds this", the
  principal for "what are its groups".

> ponytail: the TTLs and the timeout are constants. Make them settings when an
> operator shows a directory that needs others.

> ponytail: **(cluster)** one directory cache per node, so N nodes ask N times.
> Share answers through the metadata store when the directory's query rate shows
> it.

### 5.3 What a stale entry may grant

Every directory answer has an age. For its first 5 minutes it is **fresh**: it
is used, and the directory is not asked again. After that the next lookup asks
the directory. If the directory answers, the entry is fresh again. If it fails
or times out:

- an entry whose last successful answer is no older than the TTL plus
  `identity.directory_stale_grace` (default 15 minutes) **MAY** be used, each use
  counted (`identity_directory_lookups_total{result="stale"}`);
- anything else **MUST** be refused: a lookup with no entry, or one older than
  that, fails with `ErrUnavailable`, and a sign-in that needed it fails.

**The bound, stated once.** A directory account is let in at most the TTL plus
the grace after the last answer that showed it enabled, on any proof: Kerberos
with or without a PAC, NTLM pass-through, or `AUTH_SYS`. A local `Disabled` flag,
which is the store's, takes effect within §7.2's bound regardless. A grace of
zero means "never use an answer the directory has not confirmed in the last
5 minutes".

*Example.* carol's groups were fetched at 08:40. The directory goes down at
08:50. At 08:55 her entry is due, the query fails, and the entry is 15 minutes
old, so it is used. At 09:05 it is 25 minutes old and her sign-in is refused.
dave, never looked up, is refused at 08:51.

> decision: serve stale for a bounded grace, then fail closed. Failing closed at
> once turns every directory blip into a sign-in storm of refusals for users the
> server knew a minute ago; serving stale without bound lets a fired user in for
> as long as the directory is down. The grace is the window an operator accepts.
> Set it to zero where a disabled account must stop within the TTL, and lengthen
> it where the directory is known to be remote and flaky.

### 5.4 Errors never become answers

- A store or directory error **MUST NOT** be read as "no such principal". "No
  such principal" leads to anonymous for an `AUTH_SYS` uid (§4.3) and to the
  everyone grant at a share; an outage read that way grants access.
- An error **MUST NOT** fall back to the share's everyone grant, to a guest, or
  to a cached negative answer.
- Every such refusal is counted with its cause (§9.2).

| Failure | Behaviour |
| --- | --- |
| store unreachable during sign-in | `ErrUnavailable`; the sign-in fails |
| store or directory error during resolution | the call fails `ErrUnavailable`, answered as the protocol's retryable error |
| directory down, entry within grace | served stale, counted (§5.3) |
| directory down, no fresh-enough entry | refused, counted (§5.3) |
| keytab missing or unreadable | Kerberos sign-ins fail `ErrUnavailable`; NTLM and `AUTH_SYS` unaffected |
| PAC that does not verify, or names another SID than the record | `ErrLogonFailure` (§3.4) |
| context past `Expires` | the expired-credential answer, release calls aside (§3.8) |
| group closure incomplete or over the cap | the resolution fails; never truncated (§2.5) |
| `Anonymous` principal deleted | calls the policy would squash are refused (§6.2) |
| issuer's key set unfetchable past its grace | `ErrUnavailable`; the API sign-in fails (§8.2) |

### 5.5 A directory without Active Directory or Kerberos

A directory is a source of names, never a proof: it says who holds a name, a uid
or a SID, and which groups they are in, and it verifies no credential. With a
plain RFC 2307 directory, no domain controller and no KDC:

| Path | Works? | Why |
| --- | --- | --- |
| NFS `AUTH_SYS` | yes, as a claim | a claimed uid maps by `uidNumber` to the directory principal (§4.3); its groups are the claimed gids, or, with `ManageGIDs`, its closure, which includes the directory's groups (§6.3); the caller is `Claimed`, never `authenticated` |
| NFS or SMB Kerberos | only with a KDC | any KDC serving the same users will do; a ticket's name resolves through the directory by §4.2 step 3 |
| SMB NTLMv2 | no, for a user only in the directory | NTLMv2 is challenge and response, so no password reaches the server to check against the directory; a local check needs an NT hash, which no directory user has (§3.5); pass-through needs a domain controller |
| management-API sign-in | local users only | the API checks a stored password hash (§8.1); a directory user has none, and the API refuses to set one |

**So an SMB user who exists only in a directory without a domain controller
cannot sign in.** The supported path without one is the directory for
`AUTH_SYS`, and a KDC wherever a proof is needed. Giving such a user a local
password is refused: two passwords for one principal drift apart.

**A claimed uid the directory confirms mints its principal**, under three
guards:

1. **Only on the directory's word.** A record is minted only when the
   directory answers with an entry holding that `uidNumber`. A uid it does not
   know acts as anonymous (§4.3) and writes nothing.
2. **At a bounded rate.** Mints a `Claimed` caller triggers are capped per node
   (§7.3), on top of the per-address cap on the lookups, so a scan of claimed
   uids mints at most what the directory holds, at the cap's pace, whichever
   hosts send it.
3. **As a copy of the directory's entry.** The record is bound to the entry's
   permanent ID, never its name, refreshed on the directory cache's TTL, and
   disabled when the directory says the entry is disabled or answers that it
   is gone, exactly as for any directory principal
   ([§5.1](#5.1%20What%20a%20directory%20provides),
   [§3.7](#3.7%20Disabled%20and%20deleted%20principals)).

> decision: a claimed uid mints the directory principal that holds it,
> although §4.4 never mints for an unauthenticated `Find`. Owners, ACL
> entries, quotas and audit all key on one internal principal ID, so an
> `AUTH_SYS` user who also signs in over Kerberos or SMB must be one record,
> not a number beside it; without the mint no directory user is ever minted
> where nothing signs in with a proof. The guards keep the claim from buying
> more than the directory already says: no record for a number it does not
> know, a mint rate no client can raise, and a record that follows the entry.
> Windows Server's NFS service and NetApp ONTAP map a claimed uid to the
> directory account the same way. Overturned if
> `identity_mints_total{kind="user"}` is shown rising at the node cap while
> no user signs in; mints then need a proof, and `AUTH_SYS` users a record
> the operator creates.

> ponytail: NT hashes are never read from the directory (an attribute such as a
> Samba-style schema's NT password). Reading one needs a privileged bind, an NT
> hash is equivalent to the password, and §3.5 lets NT hashes exist only while
> `identity.ntlm` is on. Add it when a deployment needs NTLM for directory users
> without a domain controller, with a sealed bind credential that may read only
> that attribute and the hash held only in memory.

## 6. Resolution per share

### 6.1 From Identity to caller

Resolution is admission's step 4
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)). It runs
in `*vfs.Server`, at the primary, on every call the caller cache (§7.1) does not
answer, after the share's state, `MinProof`, `MinProtection` and client rules
have admitted the call, and before
[RFC 19](rfc-19-authorization.md#3.1%20One%20request%2C%20in%20order) decides
the file. It runs five steps, and each fills part of the `Caller`:

1. **Map** — who proved this? With proof kind `Secret` or above, the principal
   sign-in bound the evidence to (§3.1), never by its name again; with
   `Claimed`, by the claimed uid (§4.3), an unmapped uid being anonymous here,
   before step 2; with `None`, anonymous. A disabled or deleted principal is
   refused (§3.7). Fills `Mapped`, which state protection compares and which
   never changes for the life of a connection. A call that names no share,
   such as `EXCHANGE_ID` or `SETCLIENTID`, runs this step alone, under the
   installation's defaults and never a share's
   ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)).
2. **Squash** — does this share want the caller treated as somebody else
   (§6.2)? Fills `Principal`: the mapped one, or the share's `Anonymous`.
3. **Collect** — which groups is it in (§6.3)? Fills `Groups` and `DenyOnly`.
4. **Grant** — what does this share allow it (§6.4)? Fills `Access` and
   `ReadOnly`, read for the squashed principal of a claimed identity and for
   the mapped principal of a proven one (§6.2).
5. **Flags** — how far is it trusted (§6.5)? Fills `Authenticated`, `Admin`
   and `Privileged`.

![Resolution in five steps, Map, Squash, Collect, Grant and Flags, each filling its own Caller fields from Mapped through to the authenticated, admin and privileged flags. A squash replaces the principal with the share's Anonymous and empties its groups and flags, and every failure is a refusal, never a fall-back to anonymous](img/rfc18-pipeline.svg)

*Example.* bob signs in with Kerberos and opens a file on `builds`, a share with
`Squash = root`. Map: `Mapped = bob`. Squash: bob is not root, so nothing
changes. Collect: `Groups = [devs, Finance]`. Grant: the share grants `devs`
read-write, so `Access = read-write`. Flags: authenticated, not admin, not
privileged. If build01's root sends the same request over `AUTH_SYS`, Map gives
root; Squash gives anonymous; Collect gives anonymous's groups only; Grant gives
the everyone grant, if any; Flags are all false.

```go
package identity // pkg/identity

// Identity is what the adapter proved for one request, in no protocol's terms;
// pkg/vfs aliases it (RFC 17 §3.0). It names no flavour, mechanism or protocol:
// each adapter maps its own to a ProofKind and a Protection (RFC 21 §11.1,
// RFC 22 §16.1).
type Identity struct {
	Proof      ProofKind
	Protection Protection
	Claims     Claims     // what the client asserted; read only at Claimed, never as proof
	Evidence   Evidence   // what sign-in established (§3.1); zero below Secret
	Client     netip.Addr // client rules read it; the proof key leaves it out (§7.1)
}

// ProofKind is how the sender was proved, in increasing order. Secret and
// Ticket prove a principal. Prose drops the Proof and Protection prefixes.
type ProofKind uint8

const (
	ProofNone    ProofKind = iota // nothing: an anonymous or guest caller
	ProofClaimed                  // POSIX ids asserted, nothing verified
	ProofSecret                   // NTLMv2's challenge and response, and nothing else (§3.5)
	ProofTicket                   // a third party's mutual proof, such as Kerberos
)

// Protection is how the request's messages are protected, in increasing order.
type Protection uint8

const (
	ProtectionNone      Protection = iota
	ProtectionIntegrity            // every message signed
	ProtectionPrivacy              // every message sealed
)

// Claims are POSIX ids a client asserted. They name no principal until
// resolution maps them (§4.3), and they never make a caller authenticated.
type Claims struct {
	UID, GID uint32
	GIDs     []uint32
}

// Key is the proof key (§7.1): a collision-resistant digest of every field but
// Client, combining the evidence's digest computed at sign-in with the proof
// kind, the protection and the claims.
func (id Identity) Key() ProofKey

// Resolver is admission's step 4 (RFC 17 §4.9); *vfs.Server is its only caller.
// p is the share's policy, read in one snapshot (RFC 16 §3's SharePolicy);
// readOnly says admission's step 3 narrowed the call to read-only.
type Resolver interface {
	Resolve(ctx context.Context, id Identity, p metadata.SharePolicy, readOnly bool) (*Caller, error)
}

// Caller is what one call acts as on one share. Every field any check reads
// is here (RFC 7 §7.4).
type Caller struct {
	Principal     metadata.Principal   // after squashing: what the permission check and grants read
	Mapped        metadata.Principal   // before squashing: what state protection and a client record's creator compare (RFC 14); the permission check never reads it
	Groups        []metadata.Principal // the closure, sorted
	DenyOnly      []metadata.Principal // principals the proof marked deny-only (§3.4); empty when squashed
	Access        metadata.ShareAccess // the grant's level, narrowed to read where admission narrowed it (§6.4)
	ReadOnly      ReadOnlyCause        // why Access is read: NotNarrowed, Grant (the grant's own level) or Share (a client rule or the policy's ReadOnly flag)
	Authenticated bool                 // §6.5
	Admin         bool                 // §6.5
	Privileged    bool                 // §6.5; a protocol may confine it to an open (RFC 19 §3.5)
	Built         uint64               // the State it was resolved under (§7.2); no check reads it
	Fresh         time.Time            // no cache serves it after this (§7.1); no check reads it
}
```

- **The read-only cause** lets a refusal say why: a mutation refused because the
  share or a client rule is read-only is `ErrReadOnly`, which the adapters answer
  as a read-only filesystem; one refused by a `read` grant is `ErrAccess`
  ([RFC 19 §3.1](rfc-19-authorization.md#3.1%20One%20request%2C%20in%20order)).
- **Proof kind and protection.** A share's policy sets a floor on each part,
  `MinProof` and `MinProtection`
  ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)):
  Kerberos only is `MinProof` `Ticket`, and a minimum Kerberos level is
  `MinProtection`. Admission compares both; resolution reads the proof kind,
  the evidence and the claims, never the protection. Which credential carries
  which pair is [RFC 21 §11.1](rfc-21-nfs.md#11.1%20Identity)'s and
  [RFC 22 §16.1](rfc-22-smb.md#16.1%20Identity)'s.
- **Errors.** Resolution answers with RFC 17 §4.3's errors: `ErrAccess` for a
  disabled or deleted principal, a deleted `Anonymous`, or a principal over the
  group cap; `ErrUnavailable` for an undecidable lookup or an incomplete
  closure. An expired context never reaches it, because the adapter refuses a
  request on a context past `Expires` (§3.8).
- A compound or path that crosses into another share is resolved again there,
  since squashing and grants are per share.

### 6.2 Squashing

A share's `Squash` is `none`, `root` or `all`
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)).
Squashing replaces the mapped principal with the policy's `Anonymous`:

| Squash | Replaced |
| --- | --- |
| `none` | nothing |
| `root` | a caller mapped to root (§2.1): a claimed uid 0, or a name linked to root |
| `all` | every caller |

- `None` callers, and principals with no file spelling (§2.4), are the share's
  `Anonymous` whatever the setting.
- **Squashing replaces the whole principal.** A squashed caller **MUST** carry
  no group, SID or name of the principal it replaced: its groups are the
  `Anonymous` principal's own (§6.3), its flags all false. A squash that
  replaced the uid and kept the SIDs would leave every SID-keyed ACL entry
  matching.
- **Anonymous is a principal, not a number.** It defaults to the well-known
  anonymous principal (§2.1). An operator who wants the effect of
  `anonuid=1000,anongid=1000` names the user whose uid is 1000. A deleted
  `Anonymous` fails admission closed: calls the policy would squash are refused.
- Squashing applies to every protocol alike; `all` over SMB makes every session
  act as anonymous on that share. The default for a new share is `root`.

**The squash order follows the proof.** Whether the share grant is read for
the principal before or after the squash depends on what the proof proved:

- **A claimed identity** — proof `None` or `Claimed`, such as `AUTH_SYS` or
  `AUTH_NONE` — is squashed first, and its grant is read for the squashed
  principal: on an `all` share its access is the `Anonymous` principal's own
  grant, if any, else the everyone grant, and per-user grants decide nothing.
- **A proven identity** — proof `Secret` or `Ticket`, such as NTLMv2 or
  Kerberos — has its grant read for `Mapped`, the principal sign-in
  authenticated, with that principal's own groups (§6.3's `Secret` or `Ticket`
  row), before any squash. The squash then applies to file operations only:
  `Principal`, `Groups` and the flags are the `Anonymous` principal's, as for
  any squashed caller, while `Access` is the authenticated principal's grant,
  `none` included. On an `all` share, a per-user grant decides who may use it.

> decision: a claimed identity is squashed before its grant is read, a proven
> one after. A grant on a claimed uid has no security value: all-squash means
> "do not trust the claim", and reading grants first would let any client
> claim an administrator's uid and receive that principal's `admin` or
> `read-write` grant. A proven principal's grant names who the proof
> established, so reading it first lets an operator say which users may reach
> an anonymous drop box. This is the order the Linux kernel NFS server applies
> to `all_squash` and the order Samba applies to `valid users` before
> `force user`. The cost: a proven caller's grant and its file identity
> differ on a squashed share. Overturned if a proof kind between `Claimed`
> and `Secret` is added; it then needs its own row here.

> decision: squashing applies to SMB too, so a share's policy means one thing
> whatever the protocol. Over SMB `root` rarely matters, since only a principal
> linked to root meets it, and a share whose files must be anonymous to
> everyone — a public drop box — is `all` for both protocols. Revisit if an
> operator needs NFS squashed and SMB not on one share; that is two shares today.

### 6.3 Groups

| Proof kind | Groups |
| --- | --- |
| `Claimed` | the claimed gid and supplementary gids, each mapped (§4.3); with `ManageGIDs`, the mapped principal's closure instead (§3.2), which for a directory principal includes the directory's groups |
| `Secret` or `Ticket` | the closure of the principal's store memberships (§2.5), unioned with the groups of the evidence's verified SIDs and, for a directory principal, the directory's groups |
| squashed, `None`, a principal with no file spelling | the `Anonymous` principal's closure |

- The closure is computed whole or the call fails (§2.5).
- **Deny-only.** Each of the evidence's `DenyOnly` SIDs maps to a principal in
  `DenyOnly`, never in `Groups`, and adds none of its own groups. A lookup error
  while mapping one fails the resolution, as for a group: dropping a deny fails
  open. A deny-only SID no principal holds stays in the evidence, so the
  resolution after its principal is minted — which raises the generation
  (§7.2) — maps it; until then no stored entry can name it
  ([RFC 19 §2.8](rfc-19-authorization.md#2.8%20What%20a%20stored%20ACL%20may%20hold)).
- Claimed groups are the client's assertion, like its uid. A share that must not
  trust them sets `ManageGIDs`, or requires Kerberos.

### 6.4 Share access from the grants

A caller's access to a share is read from the share's grants
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)),
in this order, stopping at the first that matches:

1. a grant naming the caller's own principal — whatever it says, `none`
   included, it is authoritative. A grant of `none` **MUST** be stored as a
   grant, distinct from no grant: setting `none` blocks, and only removing the
   grant, a separate operation, lets the caller fall through to its groups;
2. the widest grant naming any of the caller's groups;
3. a grant naming **authenticated users**, for an `authenticated` caller;
4. a grant naming **everyone**;
5. otherwise `none`.

- A share's "default access" for callers no other grant names is a grant to
  everyone, or to authenticated users. There is no other default.
- `Access` is the level found, narrowed to read where admission narrowed the
  call; `ReadOnly` records which narrowed it (§6.1). What each level then allows
  on a file is the share ceiling
  ([RFC 19 §3.2](rfc-19-authorization.md#3.2%20The%20share%20ceiling)).
- The grant-list version is read on every call, and is in the caller cache's
  key (RFC 16 `S‖id‖gv`, §7.1), so a changed or deleted grant reaches a client
  that still holds handles at its next call.

> decision: a principal's own grant overrides its groups', including a `none`.
> That is how an operator blocks one user of a group without leaving the group,
> and it is the only way to express it with four access levels. The cost is
> that a user granted `read` alone misses a group's `read-write`. Revisit if
> operators are shown to expect group grants to widen a user's.

> ponytail: step 2 reads one grant per group, up to 1 024 point reads on a
> cache miss. Read the share's grant list once, under its grant-list version,
> when a profile shows misses dominated by grant reads.

### 6.5 What the proof proved: authenticated, admin, privileged

This RFC decides which callers get the three flags; what each lets a caller do
is [RFC 19 §3.5](rfc-19-authorization.md#3.5%20Privilege%20and%20the%20system-security%20right)'s,
and the entries they match are §2.1's.

| Caller | `Authenticated` | `Admin`, when its grant says `admin` | `Privileged` |
| --- | --- | --- | --- |
| `None` | no | no | never |
| `Claimed` | no | no | root, unsquashed |
| `Secret` or `Ticket`, any protection | yes | yes | root, unsquashed; or an `admin` |
| a principal with no file spelling | no | no | never: it acts as `Anonymous` (§2.4) |
| any squashed caller | no | no | never |

- `Admin` **MUST** be set only from the grant, and only for a proof of `Secret`
  or above. A uid, a group name, a SID such as `S-1-5-32-544` in a PAC, or a
  management-API role **MUST NOT** set it: any client's root can assert the
  admin's uid, so a claimed identity gains nothing from an `admin` grant.
- Where a protocol confines privilege to a handle opened with backup or restore
  intent, as SMB does, that confinement is the open's
  ([RFC 19 §3.5](rfc-19-authorization.md#3.5%20Privilege%20and%20the%20system-security%20right)),
  not the identity's: the caller's flag is the same for every protocol.
- An NTLM pass-through session is authenticated exactly as a Kerberos one: the
  domain controller verified it.

## 7. Caches

### 7.1 Every key carries the whole proof

There is **one cache of resolved callers**: admission's
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)), which
holds the `Caller` with the result of steps 1–3. Identity keeps no cache of
callers of its own, only the directory's (§5.2). This section says what that
cache's key must hold and how it behaves.

Its key starts with the **proof key**, `Identity.Key()`: a digest of
everything the request proved and claimed — the proof kind, the protection, the
bound principal, the verified name, each SID with whether it is deny-only, and
every claimed uid and gid. The protection is in it, though resolution ignores
it, because admission compares it. Only the client's address is left out, and
the key carries it separately wherever a client rule decided (RFC 17 §4.9). The
rule this buys: two requests share an entry only if they proved exactly the same
thing.

| The key **MUST** include | Because |
| --- | --- |
| the proof key | the caller is derived from what was proved |
| the share, `ExportPolicy.Version` and the share's grant-list version | squash, `ManageGIDs`, `Anonymous` and the grants are per share |
| the identity `State` it was built under (§7.2) | a principal, membership or mapping change, a changed directory record and `identity.ntlm` all arrive as a new `State` |
| the client address, where a client rule or netgroup decided | two hosts of one principal can be ruled differently |

*Example.* alice over Kerberos and an `AUTH_SYS` call sending alice's uid 1500
have different keys: the proof kind differs, and only one has a name. They never
share an entry, so the second call cannot inherit alice's `Finance` group.

- Adding a field to `Identity` or `Evidence` is an edit to `Key()`, and adding
  one to `Caller` an edit to the key. A field that is not identity — an e-mail
  address — is in no key. A key that cannot be shown to include the whole value
  **MUST** be replaced by one that does
  ([RFC 7 §7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read)).
  A digest **MAY** stand in for a key only if it is a collision-resistant hash
  of the whole key.
- **Concurrent misses of one key share one resolution**, so a storm of calls
  from one client resolves once.
- **No entry outlives its freshness.** A caller of a directory principal, or one
  that used a directory answer, is served at most 5 minutes — the directory
  TTL — after it was built (`Caller.Fresh`); a caller from evidence with an
  `Expires` no later than that. Past it, the entry is rebuilt through the
  directory cache, which answers fresh, stale within the grace, or refuses
  (§5.3).

### 7.2 Local changes take effect within a second

The installation keeps one counter, the **principals generation**. Every write
to a user, group, membership or spelling raises it, in the same transaction as
the write: the `PGEN` key
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)).
Each node reads it once a second
([RFC 23 §2.3](rfc-23-control-plane.md#2.3%20The%20node%20reconciles%3B%20it%20does%20not%20take%20orders))
and hands each new value to identity in a new `State` (`Apply`, §2.6); the node
that wrote the change applies it at once.

- **A resolution reads the `State` once**, before its first store or directory
  read, and stamps the caller with it (`Caller.Built`). A caller whose stamp is
  older than the current `State` is neither stored nor served. So a resolution
  that read bob's record before a disable, and finished after the `Apply` that
  delivered it, is not cached under the new `State`, and bob's next call misses
  and is refused. The same rule holds for the API's account check (§8.1).
- Grant and export-policy changes carry their own versions, read on every call
  and already in the key.
- The generation covers only what the store holds. Directory answers expire by
  their own clock (§5.2), and a disabled directory account is noticed when its
  answer is refreshed (§5.3).

*Example.* At 10:00:00.2 the operator disables bob; the generation goes from 41
to 42. At 10:00:01 node B reads 42. bob's next call on node B misses the cache,
re-reads his record, finds `Disabled`, and is refused, though his SMB session
from 09:00 is still open.

> ponytail: one generation for all principals, polled once a second. A change
> to one user invalidates every node's whole cache, and an installation with
> constant churn rebuilds constantly. Key entries on per-principal versions,
> pushed rather than polled, when a profile shows resolution misses after
> generation changes at the reference box's call rate.

### 7.3 Bounds

Every table here is keyed on input a client controls, so each is charged to
[RFC 24](rfc-24-resources.md#5.1%20No%20queue%20is%20unbounded)'s
connection-state pool, and past its share:

| Table | Past its share |
| --- | --- |
| caller cache | the least recently used entry is evicted, counted |
| directory cache | the least recently used entry is evicted; an evicted entry is a miss, never "not found" |
| replay cache | holds entries younger than the clock skew; a sign-in that would need one more fails `ErrUnavailable` |
| logon throttle | the oldest pair is evicted, counted |
| directory lookups a `Claimed` caller triggers | at most 100 a minute per client address; past it the call is refused with a retryable error, never mapped to anonymous |
| records a `Claimed` caller's lookup mints (§5.5) | at most 100 a minute per node, whatever the client; past it the call is refused with a retryable error, never mapped to anonymous, and nothing is written |

GSS contexts are the NFS adapter's table, bounded where RFC 21 keeps them
([RFC 21 §10](rfc-21-nfs.md#10.%20RPCSEC_GSS)).

> ponytail: a full throttle evicts its oldest pair, which frees a guesser who
> can fill it — but one who can fill it can more cheaply rotate addresses, which
> §3.1's ponytail already concedes. Refuse new pairs while every pair is at its
> limit when the logon stream shows a spray that eviction let through.

> ponytail: the per-address lookup cap and the per-node mint cap are constants.
> Make them settings when a legitimate client, or a first-day migration of
> `AUTH_SYS` users, is shown to trip them.

## 8. Management-API users

The management API has users of its own: operators who create shares, users and
grants. Their tokens, roles, sessions and sign-in throttle are
[RFC 23 §5](rfc-23-control-plane.md#5.%20Authentication%20and%20authorization)'s,
and so is the bootstrap administrator
([RFC 23 §5.4](rfc-23-control-plane.md#5.4%20The%20bootstrap%20administrator%20and%20the%20first%20password)).
This RFC fixes only where the two meet:

- **A role grants nothing on files.** An API role **MUST NOT** make a principal
  `admin`, privileged or granted on any share, and resolution never reads one.
  File access comes only from share grants (§6.4) and the proof (§6.5).
- **An API-only account is no file principal** (§2.4).
- An API user and a file principal **MAY** be one `User` record, sharing a name
  and password; the API reads its role, and file access reads its grants. A
  password change updates the NT hash with it while NTLM is on (§3.5).

> decision: API users and file principals are one `User` record kept apart by
> rule: a role is never read by resolution, and an account with no file spelling
> cannot sign in over a file protocol. Overturned if an API account is needed
> that is not a `User` record.

### 8.1 The API's sign-in interface

The management API calls identity directly, never through `vfs`: it signs in
over no file protocol and acts on no share. `node.New` hands it these and
nothing else of identity
([RFC 23 §2.1](rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components)):

```go
package identity // pkg/identity

// APISignIn is the API's account check (RFC 23 §5.3).
type APISignIn interface {
	// VerifyPassword checks name's password against its stored hash and returns
	// the account's principal. A wrong password, an unknown name, a disabled or
	// deleted account, and an account with no password all fail
	// ErrLogonFailure, alike; ErrUnavailable when the store cannot answer.
	VerifyPassword(ctx context.Context, name string, password []byte) (metadata.Principal, error)
	// Account returns p's current record for the API's per-call check, under
	// the principals generation (§7.2); ErrLogonFailure if disabled or deleted.
	Account(ctx context.Context, p metadata.Principal) (metadata.User, error)
}

// TokenVerifier verifies an OpenID Connect ID token (§8.2) and returns the
// principal its (issuer, subject) is linked to. ErrLogonFailure for a token
// that fails any check or an unlinked subject; ErrUnavailable when the
// issuer's keys cannot be had.
type TokenVerifier interface {
	VerifyToken(ctx context.Context, raw []byte) (metadata.Principal, TokenClaims, error)
}

// TokenClaims are the claims of a verified ID token the API reads. None of
// them identifies the account.
type TokenClaims struct {
	Issuer string
	Groups []string // the values of the issuer's configured role claim, for RFC 23's role map
}

// NewHashes derives what a new password is stored as: the password hash and,
// only while ntlm is on and u holds file spellings and is not a directory
// principal, the NT hash (§3.5). It is pure: it reads and writes nothing. The
// API writes the result with the record, conditional on ntlm.Revision still
// being current; for a directory principal it fails ErrInvalid.
func NewHashes(u metadata.User, password []byte, ntlm NTLMSetting) (PasswordHashes, error)
```

- `VerifyPassword` compares in constant time, and an unknown name does the same
  work as a wrong password — a hash verification against a fixed dummy — so
  timing does not tell the two apart.
- Tokens, sessions, roles, the not-before time and the API's sign-in throttle
  stay RFC 23's: `Account` returns the record and the API reads its role and
  `NotBefore` itself.
- An API sign-in emits no event to the `logon` stream; the API audits its own
  ([RFC 23 §6](rfc-23-control-plane.md#6.%20The%20audit)).

### 8.2 OpenID Connect sign-in

OpenID Connect lets an operator sign in to the management API with an account at
an outside identity provider. It never reaches file access. The provider's token
names the person by two fields: `iss`, the provider, and `sub`, the provider's
permanent ID for that person. To **link** a provider account to an installation
user, the operator stores that (`iss`, `sub`) pair on the user's record as its
`oidc` spelling
([RFC 23 §3.5](rfc-23-control-plane.md#3.5%20Users%2C%20groups%2C%20grants%20and%20netgroups)).
From then on, a valid token carrying that pair signs in as that user.

An installation **MAY** configure issuers (`identity.oidc`), each with its URL,
client ID, audience and allowed signing algorithms. `VerifyToken` **MUST** refuse
the token unless every check holds (OpenID Connect Core 1.0 §3.1.3.7):

- its signature verifies under a key of the issuer's key set (RFC 7517), named
  by the token's `kid`; a token without `kid` is refused when the set holds more
  than one key;
- its `alg` is one the installation's configuration allows, never one the
  issuer's discovery document offers on its own, and is asymmetric — `RS*`,
  `PS*`, `ES*` or `EdDSA`, never `none` or an HMAC algorithm — and it matches the
  selected key's type, and its `alg` where the key names one;
- `iss` is a configured issuer, exactly as configured;
- `aud` holds the issuer's configured audience, the installation's client ID
  unless set otherwise; when `aud` holds more than one value, `azp` is present
  and equals the client ID;
- `exp` is in the future and `nbf` and `iat`, when present, are not, each within
  a clock skew of 5 minutes;
- `nonce` matches, where the flow that got the token sent one; the device grant
  sends none.

**The issuer's keys.** Identity fetches the discovery document, refuses it unless
its `issuer` equals the configured issuer, and fetches the key set from the
`jwks_uri` it names. Every fetch — discovery, key set, token endpoint — uses TLS
with the chain verified and the host name matched, as §5.1 requires of the
directory. The key set is cached for 1 hour. A token naming a `kid` the cache
lacks refetches once, at most once a minute per issuer. When a fetch fails, the
cached set is used for at most 15 minutes past its hour, each use counted; past
that the sign-in fails `ErrUnavailable`. An unfetchable key set never yields a
session.

**From token to principal.** The subject is the `oidc` spelling (§2.2), looked
up in the protocol-ID index. Nothing else in the token identifies the account:
`email` and `preferred_username` can be passed to another person at the
provider, and **MUST NOT** be used to find, link or update a principal. An
unlinked subject is refused `ErrLogonFailure`. Group or role claims are handed
back as `Groups`, which the API maps to roles
([RFC 23 §5.5](rfc-23-control-plane.md#5.5%20OpenID%20Connect%20sign-in)); a
role still grants nothing on files.

*Example.* alice's provider account is (`https://login.example`, `00u1abc`). The
operator links it to user alice. Later the provider gives alice's old address to
bob. bob's token carries his own `sub`, so it matches no link and is refused;
alice's still signs in as alice.

> decision: the `oidc` spelling keys on (issuer, subject) only. OpenID Connect
> Core guarantees that pair stable and never reassigned; an address or a user
> name can pass to another person at the issuer, who would then inherit the
> account. Overturned if an issuer is shown to reissue subjects, which no
> conforming issuer does.

> ponytail: an unlinked subject is refused; nothing mints an API account at
> first sign-in. Add minting — an API-only user holding the `oidc` spelling and
> nothing else, with no role and a generated name — when operators are shown
> linking subjects one by one at scale.

> ponytail: the key set's hour and its 15-minute grace are constants. Make them
> settings when an issuer is shown to rotate keys faster.

> ponytail: SAML is not supported. Its bindings are browser redirects and posts,
> and the installation has no web interface. Revisit when a web console is added.

## 9. Invariants and observability

### 9.1 Invariants

| # | Invariant | Checked by |
| --- | --- | --- |
| ID1 | Every comparison of identity compares `PrincipalID`s, never a spelling, and every cache of a caller is keyed by everything it was derived from. | §10 one uid, two proofs; client in the key |
| ID2 | No ID given to a principal is given to another: `PrincipalID`, allocated uid, gid and RID alike. | §10 no reuse (RFC 16's suite) |
| ID3 | No uid or gid is ever a default; every user with a file spelling has a uid and a SID, every group a gid and a SID; a principal with no uid acts on files as `Anonymous` and no grant names it. 0 is root's and 65534 anonymous's alone. | §10 one uid, two proofs; SID-less directory; no file spelling |
| ID4 | A SID never changes once given. | §10 SID-less directory |
| ID5 | A store or directory error is never read as "no such principal", and never yields a session, a guest, anonymous or a default grant; a failed sign-in yields no session. | §10 error is not "unknown"; no map to guest |
| ID6 | `Admin` is set only from a share grant, for a proof of `Secret` or above; `Claimed` and `None` callers are never `authenticated`. | §10 admin needs proof |
| ID7 | A squashed caller carries nothing of the principal it replaced. | §10 squash drops SIDs |
| ID8 | A group closure is used whole or not at all, deny-only principals included. | §10 group cap; deny-only lookup error |
| ID9 | A directory answer is used no longer than the TTL plus the grace after it was given, read directly or through a cached caller. | §10 stale grace; directory deadline |
| ID10 | A disabled or deleted local principal is refused on every call admitted after the first `State` that follows the change, at most 1 s after it (§7.2), and no caller resolved under an older `State` is cached. | §10 disable takes effect; in-flight disable |
| ID11 | Adapters map, squash and grant nothing; every spelling goes through `vfs.Service`'s `Spell` and `Find`. | [RFC 20 §7.1](rfc-20-adapters.md#7.1%20Imports%20and%20call%20sites)'s import test |
| ID12 | A security context acts as the principal its last successful sign-in bound it to; its name is never mapped again. | §10 binding; re-authentication |
| ID13 | A deny-only SID matches deny entries only; it never grants, through an ACL or a share grant. | §10 deny-only SIDs |
| ID14 | A grant of `none` is stored, and is distinct from no grant. | §10 `none` is stored |

### 9.2 Metrics

| Answers | Metric | Type |
| --- | --- | --- |
| Sign-ins by mechanism and outcome | `identity_authentications_total{protocol, mechanism, outcome}`: `mechanism` = `kerberos`, `ntlm`; `outcome` = `ok`, `logon_failure`, `disabled`, `invalid`, `unavailable`, `throttled`, `replay` | counter |
| Sign-in latency | `identity_authentication_seconds{mechanism}` | histogram (`request`) |
| Records identity minted | `identity_mints_total{kind}`: `machine`, `user`, `group` | counter |
| Unmapped spellings | `identity_unmapped_total{scheme}`: `uid`, `gid`, `sid`, `owner` | counter |
| SIDs dropped from a PAC or pass-through reply (§3.4) | `identity_pac_sids_dropped_total` | counter |
| Calls refused at resolution | `identity_resolution_refusals_total{reason}`: `disabled`, `group_cap`, `store_unavailable`, `directory_unavailable`, `anonymous_missing`, `ntlm_off`, `lookup_cap` | counter |
| Directory lookups by result | `identity_directory_lookups_total{question, result}`: `question` = `name`, `sid`, `uid`, `gid`, `groups`, `group`; `result` = `fresh`, `queried`, `not_found`, `stale`, `refused`, `ambiguous` | counter |
| Directory query latency | `identity_directory_seconds` | histogram (`request`) |
| ID-token verifications by result | `identity_oidc_verifications_total{result}`: `ok`, `invalid`, `unlinked`, `stale`, `unavailable` | counter |
| Age of the principals generation served | `config_generation_lag_seconds{kind="principals"}`, RFC 13's gauge | gauge |

| Condition | Scope | Raised |
| --- | --- | --- |
| `directory_unreachable` | installation | degraded while lookups are served stale within the grace; unhealthy once one is refused past it; cleared by the first query the directory answers |

No metric label or span attribute carries an issuer's subject, an address or a
user name. Caller-cache hits and misses are admission's
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)).

**Spans.** `identity.authenticate` is a child of the adapter's request span.
`identity.resolve` is a child of the service-call span, opened only on a cache
miss. Each directory query is a child of whichever opened it. Attributes are the
proof kind, the outcome and the principal's ID: never a name, SID, ticket or hash
([RFC 25 §5.4](rfc-25-observability.md#5.4%20What%20is%20never%20logged)).

`identity_directory_lookups_total{result="refused"}` rising with
`identity_resolution_refusals_total{reason="directory_unavailable"}` is a
directory outage; `identity_unmapped_total{scheme="uid"}` rising alone is a
client sending uids the store does not know.

## 10. Conformance

Each check below fails silently when broken. There are two tiers. **Tier 1**
drives identity alone, through its own interfaces, against the real metadata
store and with a fake directory, KDC and domain controller, no filesystem and no
adapter. **Tier 2** runs on the composed single-node harness
([index](rfc-index.md)), through both adapters. A row marked **(cluster)** names
the RFC 15 row that covers it on two nodes. The `Minter` and allocation rules —
no ID reused, `ErrExist` on a held row, allocation inside the transaction — are
RFC 16's conformance suite's, not repeated here.

| Rule | Tier | Check | Catches |
| --- | --- | --- | --- |
| one uid, two proofs | 2 | Create two directory users with no uid, alice in group `Finance`, bob not. Assert their allocated uids differ, and that an operator write giving bob alice's uid fails. On a share admitting `sec=krb5` and `AUTH_SYS`, alice reads over Kerberos a file whose ACL grants `Finance` only: granted. From another host, over `AUTH_SYS`, send alice's uid with no gids: refused, the caller not `authenticated`, and the caller cache missed. | a defaulted uid; a cache keyed on numbers |
| client in the key | 2 | A share admits host A read-write and host B read-only and refuses host C. Send identical `AUTH_SYS` claims from A, then B, then C: A writes, B is refused writes with `ErrReadOnly`, C is refused admission, and no call hit an entry another host built. | the read-only narrowing shared across hosts |
| root | 2 | `Squash = none`, `AUTH_SYS` uid 0 with no user record: `chown` succeeds, and a file root creates is owned by root. `Squash = root`: refused. A directory entry with `uidNumber` 0: minted with an allocated uid. | root acting as anonymous; a directory supplying uid 0 |
| stale grace | 1 | Resolve alice and bob; stop the directory. Within the grace both sign in, counted stale; carol, never resolved, is refused; past the grace alice is refused. With the grace zero, alice is refused as soon as her entry passes the TTL. | fail-open on outage, or no grace |
| disabled directory account | 1 | A directory user signs in over Kerberos with a PAC. Disable them in the directory and step past the TTL: refused. Repeat with the directory down past the TTL plus the grace: refused. Repeat with the entry deleted: refused, outcome `disabled`. | a PAC keeping a disabled account in |
| error is not "unknown" | 2 | Inject a store error on the `uid` index during an `AUTH_SYS` call to a share with an everyone grant of `read-write`: a retryable error and nothing created. | an error read as "not found" |
| no map to guest | 2 | Over SMB a wrong password, an unknown user and, with the store failing, a known user: `STATUS_LOGON_FAILURE` each time and no session. An anonymous session reaches `IPC$`, and `TREE_CONNECT` to `profiles` is refused. | a failed login answered as guest |
| squash drops SIDs | 2 | `Squash = all`: a Kerberos caller whose PAC carries `Finance` reads a file whose ACL grants `Finance` only: refused. `Squash = none`: granted. | a squash that kept the SIDs |
| squash order follows the proof | 2 | `Squash = all`, a grant of `read-write` to alice, `none` to bob, and `read` to everyone. Over `AUTH_SYS` claiming alice's uid: assert a write refused (the everyone grant). Over Kerberos as alice: assert the write succeeds and the file is owned by `Anonymous`; as bob: assert refused at the share. | a claimed uid receiving its principal's grant, or a proven principal's grant ignored on a squashed share |
| admin needs proof | 2 | A grant names uid 1500 `admin`. Over `AUTH_SYS` as uid 1500, read a mode-0600 file of another user: refused. Give the principal a Kerberos name; over `sec=krb5`: granted. `S-1-5-32-544` in a test PAC for a non-admin: not `admin`. | `admin` from a uid or a SID |
| roles grant nothing | 2 | [RFC 23 §12](rfc-23-control-plane.md#12.%20Conformance)'s §5.1 check, which also asserts that the account holds no spelling. | — |
| disable takes effect | 2 | bob holds an SMB open and an NFS GSS context; make one call on each, a cache hit. Disable bob; before the `State` is applied, the next call is still served; apply it (fake clock): every call from both is refused. | a cached caller outliving a disable |
| in-flight disable | 1 | Block bob's record read in a fake store. Disable bob and apply the new `State`. Release the read: nothing cached under the new `State`, and bob's next call misses and is refused. | a stale resolution stamped with the new generation |
| group cap | 1 | A principal in 1 025 groups: refused, never resolved with fewer. A nested group whose read fails mid-closure: the call fails. A membership cycle: resolved. | truncated group lists; a cycle read as a failure |
| owner strings | 2 | Over `sec=krb5`, `chown` to `alice@corp.example` and to `alice@other.example`: success, then `NFS4ERR_BADOWNER`. Over `AUTH_SYS`, `GETATTR`: numeric owners. A file owned by a deleted user: `nobody@…`, and over SMB `S-1-0-0`. | names mapped by guesswork |
| PAC | 1 | A ticket whose PAC server signature is corrupted: refused. A ticket with no PAC, and one with a PAC holding no logon-information buffer, for a user in a store group: accepted, the group applies. A PAC carrying a SID of the installation's own domain for a local group with a share grant: that SID dropped, the grant not given. | an unverified or unfiltered PAC trusted; every modern KDC refused |
| PAC bound to the record | 1 | Mint directory user bob; delete bob in the directory, create a new bob with a new SID, and sign the new bob in with a PAC: refused or minted anew, never the old principal. A local alice and a directory alice in a keytab realm: refused. A PAC whose client name differs from the ticket's: refused. | a reused name bound to the old person |
| deny-only SIDs | 2 | A test PAC marks `Finance` deny-only. A file's ACL allows `Finance` read: refused. Another allows alice and denies `Finance` read: refused. A share whose only grant names `Finance`: the share refuses alice. | a deny-only SID dropped, or treated as a group |
| deny-only lookup error | 1 | Inject a store error on the `sid` index while mapping a PAC's deny-only SID: the call fails, never resolved without the deny. | a deny dropped by an error |
| binding | 2 | Over SMB, alice holds a Kerberos session. Delete alice and create a user named alice: every call on the old session refused; a fresh sign-in acts as the new principal. Repeat with an NTLM pass-through session, its user SID re-linked. | a live context re-mapped by name |
| re-authentication | 2 | alice re-authenticates her SMB session with a wrong password: the session gone. As alice with a new ticket: trees and opens kept, the new `Expires` in force. As bob: the session acts as bob, and alice's open keeps its grant. Past `Expires`, `CLOSE` and an unlock succeed and a read is refused. | a failed proof leaving the old identity in place; a client unable to release what it holds |
| pass-through | 1 | A directory user with no `krb` row signs in by NTLM pass-through: the caller is the principal holding its verified user SID; a wrong password reaches the fake domain controller once. | no mapping path; amplified lockouts |
| no file spelling | 2 | On a share whose only grant is authenticated users read, a machine principal reads a file: refused, not `authenticated`. A second machine's state operation on the first's `SP4_MACH_CRED` client record: refused. A grant naming a machine or an API-only account: refused. | a machine treated as an authenticated file caller |
| `none` is stored | 2 | Grant `Finance` read-write and alice `none`: alice refused. Remove alice's grant: alice granted through `Finance`. | `none` stored as no grant |
| directory deadline | 1 | Resolve bob over NTLM; remove bob from a directory group. Keep the session open past the TTL: his next call drops the group, with no generation change. | a cached caller outliving the directory answer |
| SID-less directory | 1 | An RFC 2307 entry with no SID: the minted principal holds a local SID from the RID counter, unchanged when the directory later adds one. | a directory principal with no SID |
| logon throttle | 1 | 10 wrong passwords for alice from one address, then the right one: refused, outcome `throttled`, and the fake domain controller saw 10 attempts, not 11. | a throttle that still checks the credential |
| expiry | 2 | Over `sec=krb5`, keep a context open past its ticket's `Expires`: the next request is refused with the expired-credential answer. | a context served past its ticket |
| replay | 1 | The same AP-REQ twice: the second refused, outcome `replay`. | no replay cache |
| keytab rollover | 1 | During a rollover, tickets under the old and the new key version are both accepted; past the retention, an old-version ticket is refused. | a rotation failing tickets, or a retired key accepted forever |
| `ManageGIDs` | 2 | A user in 20 store groups calls over `AUTH_SYS`: a file whose ACL grants only the 20th group is granted. | groups capped at the credential's 16 |
| NTLM off | 2 | Make a cache hit on a local NTLM session and on a pass-through one, then turn `identity.ntlm` off with no principal change and apply it: both refused on their next call, a fresh NTLM sign-in refused, and once the deletion ends, no NT hash in the store. Hold a password set between `NewHashes` and its write across the change: no NT hash written. | an NTLM context outliving the setting; a stray hash |
| NTLM MIC | 1 | An AUTHENTICATE with a corrupted MIC, and one with the MIC zeroed while `MsvAvFlags` marks it present: both `ErrLogonFailure`. | the drop-the-MIC relay |
| NTLM domain | 1 | A standalone installation's CHALLENGE advertises its own NetBIOS name; an AUTHENTICATE naming it in another case: verified locally. | every standalone login refused |
| no local password for a directory user | 1 | `NewHashes` for a directory principal: `ErrInvalid`, nothing stored; a local NTLM sign-in naming one: refused. | two passwords for one principal |
| directory TLS | 1 | A record asking for plain LDAP, or for skipping verification: refused at validation. A directory behind a self-signed certificate, then one whose host name does not match: no bind sent, lookups `ErrUnavailable`, nothing cached. A referral to another host: not followed. | a bind credential handed to whoever answers |
| one query per key | 1 | 100 concurrent lookups of one key against a counting fake directory: 1 query. | a sign-in storm multiplied per connection |
| exact queries | 1 | Look up a name holding `*`, `(`, `)` and `\`: the fake directory receives the escaped filter. Two entries answering one name: `ErrUnavailable`, outcome `ambiguous`, nothing bound. | a name matching another entry |
| lookup cap | 1 | One client address sends 101 distinct unknown uids in a minute: at most 100 directory queries, the rest refused retryably, none mapped to anonymous. | a uid spray loading the directory |
| anonymous lookup | 2 | Over an anonymous session on `IPC$`, `LookupSids` of an unminted directory SID: no record written. | an unauthenticated caller minting principals |
| name case | 1 | Two users whose names differ only in case: the second refused as a duplicate. | names unique byte for byte |
| `AUTH_SYS` from the directory | 2 | An `AUTH_SYS` call sends the `uidNumber` of a directory entry with no record: the caller is that entry's minted principal, not `authenticated`. | a directory that never maps a claimed uid |
| mint from a claim, guarded | 2 | Send `AUTH_SYS` calls for 10^4 uids the directory does not hold: assert each acts as anonymous and no record is written. Send calls for 500 `uidNumber`s it holds, from 50 addresses at once: assert no more than the node cap minted per minute, the rest refused retryably, never anonymous. Rename a minted entry in the directory: assert the same `PrincipalID` with the new name after the TTL. Disable one entry and delete another: assert both refused after the TTL. | a mint on the client's word, a scan bounded only per address, or a record that follows the entry's name |
| mint conflict | 1 | Mint directory user alice, then sign in bob, whose entry has another SID and alice's `uidNumber`: the sign-in fails, bob is bound to no principal, alice's record unchanged. Two concurrent mints of one entry: both bind to one principal. | a lost mint bound to whoever holds the row |
| NTLM without a domain controller | 2 | With a directory and no pass-through, an NTLM sign-in for a user only in the directory: `ErrLogonFailure`, and no session. | a directory-only user let in |
| token checks | 1 | Against a test issuer, ID tokens with the wrong issuer, the wrong audience, several audiences and another `azp`, an expired `exp`, an `nbf` ahead past the skew, a corrupted signature, `alg` `none`, `alg` `HS256` keyed with the issuer's public key bytes, `RS256` naming an EC key, and no `kid` against a two-key set: each refused `ErrLogonFailure`. A discovery document whose `issuer` differs: refused. | a check skipped; algorithm confusion |
| key-set outage | 1 | Sign in once, then make the key set unreachable under a fake clock: within the hour plus 15 minutes sign-in succeeds, counted stale; past it `ErrUnavailable` and no session. An issuer behind a certificate that does not verify: refused. | fail-open on an outage |
| unlinked subject | 1 | A subject linked to no user: refused. Link alice's subject; give alice's old e-mail address to bob's subject at the issuer: alice's next sign-in is the same principal, bob's is refused. | a subject let in unlinked; linking by e-mail |
| forwarded identity **(cluster)** | — | [RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)'s rows cover forwarding; this suite adds a unit row: `Resolve` of an `Identity` with evidence at `Claimed` is `ErrInvalid`. | a gateway bug becoming an impersonation |

**Fuzz.** `FuzzPAC`, `FuzzAPReq`, `FuzzSID` and `FuzzOwnerString` never panic,
never allocate past the input's size times a constant, and decode∘encode is the
identity on what decodes; `FuzzPAC` includes PACs with no logon-information
buffer. The NTLM message decoder is RFC 20's. **Unit.** Resolution's five steps
run as one table-driven test over (proof kind × squash × grant × no file
spelling) against a fake store; `NewHashes` is a pure function with a table of
its own.

**Who fakes what.** `*vfs.Server`'s unit tests fake identity with
`identitytest.Static`. Adapter tests fake only `vfs.Service`, sign-in included
([RFC 20 §7](rfc-20-adapters.md#7.%20Tests)), never identity. The management
API's sign-in is tested against the real identity server, called directly
([RFC 23 §12](rfc-23-control-plane.md#12.%20Conformance)).

**Suites.** Tier 1, identity alone with a fake directory, KDC and domain
controller, and Tier 2, through both adapters on the composed single-node
harness, run in `unit`, with the fuzz seeds and table tests — Tier 2 is the
in-process layer, not RFC 17/19/20's kernel-client rows, which run in
`e2e/cross-protocol`. The allocation rules are RFC 16's and the roles row
RFC 23's; the forwarded-identity row's cluster half has no suite yet.

## 11. Migration

Each slice ships alone, behind the one before it, and deletes what it replaces.

| Slice | What moves | What is deleted | Needs |
| --- | --- | --- | --- |
| 0, now | fixed in today's code: no default uid, the bootstrap admin without uid 0, no guest on a failed or unparsable setup, plain LDAP refused; the PAC failing its signature refused | the 1000 default; `root_to_admin`, `all_to_admin` | — |
| 1 | the pure packages: the `pkg/identity` value types and `Key()`, the SID codec, owner strings, the closure walk, `NewHashes`, the throttle, each with its unit and fuzz tests | — | — |
| 2 | the store: `Minter` with allocation inside its transaction, the uid, gid and RID counters, `PGEN`, the well-known IDs including root, `SharePolicy` (RFC 16) | — | slice 1 |
| 3 | SIDs: today's uid-derived SIDs written once as stored values, read beside the derived ones in a shadow pass that logs every disagreement, then switched, so no SID changes (ID4) | RID derivation from a uid | slice 2 |
| 4 | resolution in `*vfs.Server` ([RFC 17 §9](rfc-17-vfs.md#9.%20Migration) slice 2), run in shadow beside today's resolver until their callers agree, then switched: squash `none`/`root`/`all`, stored `none` grants, one caller cache | the NFS share-permission resolver, the NFSv3 auth-context cache, SID share permissions, `default_permission` | slices 2, 3 |
| 5 | the authenticator behind `SignIn`: Kerberos with the PAC binding, NTLM with the MIC, re-authentication, logon events | the five-domain NTLM retry; the RPCSEC_GSS "unmapped → nobody" path | slice 4 |
| 6 | the directory client and its cache with stale grace and verified TLS; `TokenVerifier` and OpenID Connect | the resolver cache's error TTL; `allow_plaintext` | slice 4 |

## 12. Open questions

The deferrals of §2.5, §3.3, §4.3, §5.1, §5.2, §7.2 and §7.3 are recorded at
their markers, with what would overturn each.

1. **Name case.** Whether case-insensitive uniqueness of names (§4.6) holds for
   every directory a deployment uses, or a directory with case-distinct names
   needs an exception.
2. **One machine account, several nodes.** Whether two nodes holding secure
   channels to domain controllers under one machine account invalidate each
   other's channels (§3.5) is unmeasured; if they do, pass-through needs one
   machine account per node, or one node holding the channel for all.

## Appendix A — prior art

*Non-normative.*

- **Samba's winbind** maps Windows SIDs to Unix IDs through idmap backends:
  `rid` (a range base plus the RID), `ad` (RFC 2307 attributes) and `tdb`
  (allocation, stored). §4.3 keeps the `ad` rule with allocation as its
  fallback, and writes a number once, never recomputing it. Its
  `map to guest = bad user` is the behaviour §3.6 refuses.
- **The Linux NFS server** trusts `AUTH_SYS` IDs as sent, applies `root_squash`,
  `all_squash`, `anonuid` and `anongid` per export, and with
  `rpc.mountd --manage-gids` replaces the credential's groups with the server's.
  `rpc.idmapd` and `nfsidmap` map `owner@domain` strings, refusing other
  domains, and numeric strings are used on `AUTH_SYS` by default.
- **SSSD** caches directory answers with an entry timeout and serves cached
  entries while the directory is unreachable (`offline_credentials_expiration`),
  the model for §5.3's grace.
- **Windows** builds an access token from the PAC at sign-in, so group changes
  apply at the next sign-in, and caps a token at 1 024 SIDs, so an account in
  more groups than about 1 015 cannot sign in, where §2.5's cap sits. It maps a
  failed sign-in to no session unless the Guest account is enabled, which
  current releases disable by default.
- **MS-PAC** defines the server and KDC signatures; a file server verifies the
  server signature with its own key and may ask a domain controller to verify
  the KDC signature.

## Appendix B — where the current code differs

*Non-normative.* The code on the branch this RFC was written against.

| § | This RFC | The code today |
| --- | --- | --- |
| §2.3 | no default uid | the identity resolver defaults a user's missing uid and gid to 1000 (`pkg/adapter/identity.go`); the NFSv3 auth-context cache key had to add the SIDs because of it (`internal/adapter/nfs/v3/handlers/doc.go`) |
| §2.3, §4.4 | a RID from a counter, never from a uid | local SIDs are derived from the uid by the Samba formula, RID = 2·uid + 1000 (groups 2·gid + 1001), under a random machine SID stored as a setting (`pkg/auth/sid/mapper.go`); uid 0 maps to `S-1-5-32-544`; changing a uid changes the SID |
| §2.1 | root is a well-known principal | a claimed uid 0 is root without a user record when unsquashed (`internal/adapter/nfs/auth/share_permission.go`) |
| §4.3 | directory uids written once | foreign SIDs get a durable, never-remapped Unix ID (`models.SIDMapping`); LDAP `rid` mode sets uid = gid = RID with no range base (`pkg/identity/ldap/provider.go`) |
| §2.4, §8 | API roles grant nothing on files | the bootstrap administrator is created with uid 0 so it bypasses file permission checks (`pkg/controlplane/models/admin.go`); roles themselves are read only by API middleware |
| §6.2 | squash none, root, all; anonymous is a principal; a claimed identity squashed before its grant is read, a proven one after | five modes — `none`, `root_to_admin`, `root_to_guest`, `all_to_admin`, `all_to_guest` — with anonymous uid and gid numbers per export, default 65534 (`pkg/controlplane/models/permission.go`); squashing runs in the NFS adapter and clears SIDs; grants are resolved for the pre-squash identity |
| §4.3 | an unmapped uid is anonymous | an unknown uid is a guest granted the share's `default_permission`, or a direct SID grant matched on its uid (`internal/adapter/nfs/auth/share_permission.go`) |
| §6.4 | default access is an everyone grant | a per-share `default_permission` field; per-user grants, per-group grants and SID grants (`models.SIDSharePermission`) combined with the explicit user grant authoritative |
| §3.3 | unknown Kerberos user refused | over RPCSEC_GSS an unmapped principal acts as `nobody` (`internal/adapter/nfs/rpc/gss/framework.go`); over SMB it is refused |
| §3.4 | a PAC that fails its signature fails the sign-in | the sign-in proceeds with no SIDs (`internal/auth/kerberos/service.go`), which also drops any deny-only SIDs |
| §2.4 | machine principals minted | no machine principals; service names have their prefix stripped when `StripRealm` is set (`pkg/auth/kerberos/identity.go`) |
| §3.5 | NTLMv2 against the domain as sent; the MIC enforced | five domain spellings are tried in turn (`internal/adapter/smb/handlers/session_setup.go`); the AUTHENTICATE MIC is checked but not enforced; pass-through via NETLOGON is tried only for users not in the store (`internal/auth/netlogon`) |
| §3.5 | no NT hash with NTLM off | NT hashes are stored hex-encoded on the user record, separate from the bcrypt password hash, whatever the NTLM setting (`pkg/controlplane/models/user.go`) |
| §3.6 | anonymous only, never on failure | a session is guest when no mechanism is recognised, an NTLM message does not parse, or no user store is configured; guest sessions are on by default (`session_setup.go`); an unknown user with a store is refused |
| §4.5 | names on Kerberos, numbers on `AUTH_SYS` | owner strings are always numeric in production, since the name mapper is set only in tests (`internal/adapter/nfs/v4/attrs/encode.go`); decoding accepts any domain for a numeric string, and `nobody`, `root`, `wheel` and `nogroup` by name |
| §4.6 | `lsarpc` name lookups | `LookupSids` answers; there is no `LookupNames`; unknown SIDs give `STATUS_SOME_NOT_MAPPED`, never `NONE_MAPPED` (`internal/adapter/smb/rpc/lsarpc.go`) |
| §5.1 | an ambiguous answer refuses | an LDAP lookup matching more than one entry warns and uses the first (`pkg/identity/ldap/provider.go`) |
| §5.2 | directory cache, stale grace | one resolver cache with 5 min positive, 30 s negative and 10 s error TTLs, merged concurrent lookups, no stale-serving (`pkg/identity/cache.go`); an LDAP error fails the lookup closed |
| §5.1 | verified TLS required | `allow_plaintext` permits plain LDAP, and certificate verification may be skipped (`pkg/identity/ldap/config.go`); the bind password is stored in plain text (RFC 13 Appendix A, C5) |
| §5.4 | errors never become answers | an NLM store error authorizes the lock as anonymous (`pkg/adapter/nfs/nlm.go`) |
| §7.2 | local changes within a second | the NFSv3 auth-context cache lives 5 min and nothing invalidates it on a user change, so a disabled user keeps NFSv3 access until expiry (`share_permission.go` says so) |
| §3.2 | `ManageGIDs` | not implemented; supplementary gids are taken from the credential |
| §2.2 | names unique case-insensitively | user and group names are unique byte for byte |
